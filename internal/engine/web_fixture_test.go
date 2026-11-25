package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/config"
	"github.com/NotRllyRn/cpa-pulse/internal/mcp"
	"github.com/NotRllyRn/cpa-pulse/internal/providers"
	"github.com/NotRllyRn/cpa-pulse/internal/quota"
	"gopkg.in/yaml.v3"
)

// TestWebPreviewFixture exercises management DTOs using real history storage.
// CPA_PULSE_WEB_FIXTURE_DIR optionally exports snapshots for browser tests.
func TestWebPreviewFixture(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c := &clock{t: now.Add(-6 * time.Hour)}
	e, _ := newTestEngine(t, standardHost(c), c)
	raw := map[string]any{
		"bark_url":      "https://api.day.app/preview-token/",
		"webhook":       map[string]any{"url": "", "method": "post"},
		"account_order": []string{"claude.json", "ag.json", "codex-a.json"},
		"ignition":      map[string]any{"enabled": false},
		"mcp_listen":    "127.0.0.1:0",
	}
	parse := func() config.Config {
		t.Helper()
		b, err := yaml.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := parse()
	e.Configure(cfg, nil)
	if _, err := e.loadCreds(cfg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= 72; i++ {
		c.t = now.Add(time.Duration(i-72) * 5 * time.Minute)
		for _, account := range []struct {
			index, key, label string
			remaining         float64
		}{
			{"1", "gpt", "GPT", 0},
			{"2", "gpt", "GPT", 99.6},
			{"3", "claude", "Claude", 15},
			{"4", "gemini", "Gemini", 100},
			{"4", "cg", "Claude / GPT", 65},
		} {
			remaining := 100 - (100-account.remaining)*float64(i)/72
			windows := []quota.Window{{ID: quota.WindowSevenDay, Label: "7d", Period: 7 * 24 * time.Hour, Remaining: account.remaining, Reset: now.Add(3 * 24 * time.Hour)}}
			if account.index == "2" {
				windows = append(windows, quota.Window{ID: quota.WindowThirtyDay, Label: "30 days", Period: quota.ThirtyDays, Remaining: 72, Reset: now.Add(20 * 24 * time.Hour)})
			}
			// The second Codex account loses its 5h window two hours ago.
			if account.index != "2" || i < 48 {
				windows = append(windows, quota.Window{ID: quota.WindowFiveHour, Label: "5h", Period: 5 * time.Hour, Remaining: remaining, Reset: now.Add(time.Hour)})
			}
			e.applyGroups(context.Background(), cfg, e.creds[account.index], []quota.RawGroup{{Key: account.key, SourceLabel: account.label, Windows: windows}}, quota.SourceActive, c.t, c.t, false)
		}
	}
	// Restore both Antigravity groups in the final active snapshot.
	e.applyGroups(context.Background(), cfg, e.creds["4"], []quota.RawGroup{
		{Key: "gemini", SourceLabel: "Gemini", Windows: []quota.Window{{ID: quota.WindowFiveHour, Label: "5h", Period: 5 * time.Hour, Remaining: 100, Reset: now.Add(time.Hour)}}},
		{Key: "cg", SourceLabel: "Claude / GPT", Windows: []quota.Window{{ID: quota.WindowFiveHour, Label: "5h", Period: 5 * time.Hour, Remaining: 65, Reset: now.Add(time.Hour)}}},
	}, quota.SourceActive, now, now, false)
	e.noteModels([]string{"gpt-5.6-luna", "gpt-6.1-terra", "claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5", "gemini-3.8-flash", "gemini-4-neon"}, nil)
	for _, group := range e.groups {
		candidates := rankedModels(e.priceManager(), providers.Lookup(group.Provider), group.SourceLabel, e.availableModels, "")
		if len(candidates) > 0 {
			ts := e.state.Target(group.Key)
			ts.LastSuccessfulModel, ts.ConfirmedModel = candidates[0], candidates[0]
			e.publishModelState(group.Key)
		}
	}
	e.running, e.lastPoll, e.nextPoll = true, now, now.Add(5*time.Minute)
	listener := mcp.New(NewMCPBackend(e), "0.8.0-preview")
	defer listener.Close()
	snapshots := map[string]any{}
	capture := func(name string) {
		t.Helper()
		cfg := parse()
		e.Configure(cfg, nil)
		listener.Configure(cfg.MCPEnabled, cfg.MCPListen, cfg.MCPKey)
		e.refreshTargets(cfg)
		status := e.Status()
		kinds := map[string]bool{}
		for _, target := range status.Targets {
			for _, window := range target.Windows {
				kinds[window.Kind] = true
			}
		}
		if !kinds["5h"] || !kinds["7d"] || !kinds["30d"] {
			t.Fatalf("preview requires all window schedules: %v", kinds)
		}
		status.MCP = listener.State()
		if status.Config.Webhook.URL != "" || status.Config.BarkURL != "set" || status.Config.MCPKey != "" {
			t.Fatalf("unexpected redaction: %+v", status.Config)
		}
		b, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		var saved map[string]any
		if err := json.Unmarshal(b, &saved); err != nil {
			t.Fatal(err)
		}
		snapshots[name] = map[string]any{"raw": saved, "status": status, "prices": e.Prices()}
		found := false
		for _, account := range status.Accounts {
			for _, group := range account.Groups {
				if group.EstimatedCheapestModel != "" && group.EstimatedCheapestCostUSD > 0 && group.ConfirmedModel != "" && len(group.AvailableModels) > 0 {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("pricing fixture requires a priced available model and confirmed success")
		}
	}
	capture("default")
	raw["mcp_enabled"], raw["mcp_key"] = true, "cpa_pulse_preview_fixture"
	capture("enabled")
	if !listener.State().Running {
		t.Fatalf("MCP fixture failed to start: %+v", listener.State())
	}
	raw["notice_threshold"], raw["low_threshold"], raw["critical_threshold"] = 20, 20, 10
	capture("equal")
	raw["cpa_base_url"] = "http://127.0.0.1:8317/"
	capture("url")
	raw["account_order"] = []string{"codex-b.json", "claude.json", "ag.json", "codex-a.json"}
	capture("reordered")
	raw["mcp_key"] = ""
	capture("empty-key")
	if listener.State().Running || listener.State().Error == "" {
		t.Fatal("empty MCP key must stop the listener and report an error")
	}
	histories := map[string]HistoryResponse{}
	for _, kind := range []string{"5h", "7d", "30d"} {
		history, err := e.History(6*time.Hour, kind)
		if err != nil || len(history.Series) == 0 {
			t.Fatalf("%s history: %v", kind, err)
		}
		histories[kind] = history
	}
	declining := false
	for _, series := range histories["5h"].Series {
		if len(series.Points) > 1 && series.Points[0][1] > series.Points[len(series.Points)-1][1] {
			declining = true
		}
	}
	if !declining {
		t.Fatal("burn-rate preview requires declining quota history")
	}
	retired := false
	for _, series := range histories["5h"].Series {
		if series.AuthIndex == "2" {
			retired = len(series.Points) > 0 && series.Points[len(series.Points)-1][0] < float64(now.Add(-2*time.Hour).Unix())
		}
	}
	if !retired {
		t.Fatal("history must keep the inactive Codex window")
	}
	if dir := os.Getenv("CPA_PULSE_WEB_FIXTURE_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		b, err := json.MarshalIndent(map[string]any{"snapshots": snapshots, "history": histories, "prices": e.Prices()}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "backend-fixture.json")
		if err := os.WriteFile(file, b, 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("exported real Status/History JSON: %s", file)
	}
}
