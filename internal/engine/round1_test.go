package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/host"
	"github.com/NotRllyRn/cpa-pulse/internal/ignite"
	"github.com/NotRllyRn/cpa-pulse/internal/pricing"
	"github.com/NotRllyRn/cpa-pulse/internal/providers"
	"github.com/NotRllyRn/cpa-pulse/internal/quota"
	"github.com/NotRllyRn/cpa-pulse/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestStrictOverridesBypassRefusalCooldown(t *testing.T) {
	for _, level := range []string{"account", "provider"} {
		t.Run(level, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, testZone)}
			rejected := &host.Error{Status: 404, Code: "model_not_found"}
			h := claudeIgnitionHost(c, rejected)
			h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
				return pluginapi.HostModelExecutionResponse{}, rejected
			}
			e, _ := newTestEngine(t, h, c)
			cfg := e.config()
			model := "claude-haiku-5-5"
			serveModels(t, &cfg, []string{model})
			chosen := pricingTarget(t, e, cfg)
			e.noteModelFingerprint(chosen.cred, e.credentialFingerprint(chosen.cred))
			at := c.t.Add(-time.Hour)
			e.refused[chosen.group.Key+"\x00"+model] = at
			if level == "account" {
				cfg.AccountModels = map[string]map[string]string{chosen.cred.ID: {chosen.group.Key: model}}
			} else {
				p := cfg.Provider("claude")
				p.Model = model
				cfg.Providers["claude"] = p
			}
			for i := 0; i < 2; i++ {
				got, _, err := e.execute(context.Background(), cfg, chosen)
				if got != model || err == nil || errors.Is(err, ignite.ErrNoModel) || len(h.executed) != i+1 {
					t.Fatalf("model=%s attempts=%d err=%v", got, len(h.executed), err)
				}
				if !e.refused[chosen.group.Key+"\x00"+model].Equal(at) {
					t.Fatal("override rejection refreshed refusal")
				}
			}
			delete(e.refused, chosen.group.Key+"\x00"+model)
			_, _, _ = e.execute(context.Background(), cfg, chosen)
			if len(e.refused) != 0 {
				t.Fatal("override rejection created refusal")
			}
			e.refused[chosen.group.Key+"\x00"+model] = at
			e.publishModelState(chosen.group.Key)
			e.Configure(cfg, nil)
			for _, a := range e.Status().Accounts {
				for _, g := range a.Groups {
					if g.Key == chosen.group.Key && (g.ManualModel != model || g.EstimatedCheapestModel != model) {
						t.Fatalf("override status=%+v", g)
					}
				}
			}
		})
	}
}

func TestRollingBootstrapKeepsFixedResetEligible(t *testing.T) {
	for _, period := range []time.Duration{quota.FiveHours, quota.SevenDays, quota.ThirtyDays} {
		t.Run(period.String(), func(t *testing.T) {
			w := expiredPulseWindow(period)
			w.Reset = w.Reset.Add(period + time.Minute)
			e, h, c := windowEngine(t, []quota.Window{w}, map[string]bool{w.ID: true})
			c.add(time.Minute)
			w.Reset = c.t.Add(period)
			e.applyGroups(context.Background(), e.config(), e.creds["1"], []quota.RawGroup{{Key: "codex:main", SourceLabel: "GPT", Windows: []quota.Window{w}}}, quota.SourceActive, c.t, c.t, false)
			key := "codex:1:codex:main"
			ws := e.state.Target(key).Windows[w.ID]
			if !ws.RollingReset {
				t.Fatal("fixture did not establish rolling reset")
			}
			if !e.performDue(context.Background(), e.config()) || len(h.executed) != 1 {
				t.Fatalf("bootstrap attempts=%d", len(h.executed))
			}
			if ws.LastSuccessEpoch == 0 || ws.PulsedResetEpoch != 0 || ws.RollingReset {
				t.Fatalf("bootstrap state=%+v", ws)
			}
			e.refreshTargets(e.config())
			if due := e.Status().Targets[0].NextDue; !due.Equal(w.Reset.Add(3 * time.Second)) {
				t.Fatalf("fixed reset due=%v want=%v", due, w.Reset.Add(3*time.Second))
			}
			c.t = w.Reset.Add(3 * time.Second)
			if !e.performDue(context.Background(), e.config()) || len(h.executed) != 2 {
				t.Fatal("bootstrap consumed actual reset")
			}
		})
	}
}

func TestPartialConfirmationRetriesWithoutSuccessHold(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.SevenDays)
	e, h, c := windowEngine(t, []quota.Window{five, long}, map[string]bool{five.ID: true})
	cfg := e.config()
	cfg.Ignition.PostSuccessHoldSeconds = 3600
	if !e.performDue(context.Background(), cfg) {
		t.Fatal("missing initial attempt")
	}
	ts := e.state.Target("codex:1:codex:main")
	if ts.LastSuccessEpoch != 0 || ts.LastSuccessfulModel == "" || ts.Windows[five.ID].LastSuccessEpoch == 0 || ts.RetryAtEpoch == 0 {
		t.Fatalf("partial state=%+v", ts)
	}
	c.t = epochTime(ts.RetryAtEpoch).Add(time.Second)
	if !e.performDue(context.Background(), cfg) || len(h.executed) != 2 {
		t.Fatalf("partial retry held: attempts=%d state=%+v", len(h.executed), ts)
	}
}

func TestExhaustedShortPublishesSafeRetryTime(t *testing.T) {
	for _, gate := range []string{"none", "rolling", "cooldown", "retry", "credential", "long-exhausted", "unknown-short"} {
		t.Run(gate, func(t *testing.T) {
			five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.SevenDays)
			five.Remaining = 0
			five.Reset = five.Reset.Add(time.Hour)
			if gate == "unknown-short" {
				five.Reset = time.Time{}
			}
			if gate == "long-exhausted" {
				long.Remaining = 0
				long.Reset = long.Reset.Add(quota.SevenDays)
			}
			e, h, c := windowEngine(t, []quota.Window{five, long}, nil)
			cfg := e.config()
			ts := e.state.Target("codex:1:codex:main")
			want := five.Reset.Add(3 * time.Second)
			switch gate {
			case "rolling":
				ts.Windows[five.ID].RollingReset = true
			case "cooldown":
				e.creds["1"].CooldownUntil = c.t.Add(2 * time.Hour)
				want = c.t.Add(2*time.Hour + 3*time.Second)
			case "retry":
				ts.RetryAtEpoch = float64(c.t.Add(2 * time.Hour).Unix())
				want = c.t.Add(2 * time.Hour)
			case "credential":
				e.state.Credential("1").Reason = "auth"
			}
			e.refreshTargets(cfg)
			due := e.Status().Targets[0].NextDue
			if gate == "credential" || gate == "long-exhausted" || gate == "unknown-short" {
				if !due.IsZero() {
					t.Fatalf("blocked target due=%v", due)
				}
			} else if !due.Equal(want) {
				t.Fatalf("due=%v want=%v", due, want)
			}
			if e.performDue(context.Background(), cfg) || len(h.executed) > 0 {
				t.Fatal("executed while exhausted")
			}
		})
	}
}

func TestTargetsAndModelChoicesExcludeMonitorOnlyGroups(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, testZone)}
	e, _ := newTestEngine(t, standardHost(c), c)
	cfg := e.config()
	e.poll(context.Background(), cfg, c.t, "", false)
	p := cfg.Provider("codex")
	p.Ignite = false
	cfg.Providers["codex"] = p
	p = cfg.Provider("antigravity")
	p.Groups = []string{"Claude"}
	cfg.Providers["antigravity"] = p
	e.Configure(cfg, nil)
	e.refreshTargets(cfg)
	for _, view := range e.Status().Targets {
		group := e.groups[view.Key]
		if !groupIgnitable(cfg, group.Provider, group.SourceLabel) {
			t.Fatalf("disabled target=%+v", view)
		}
		pulseable := false
		for _, w := range group.Windows {
			pulseable = pulseable || providers.Pulseable(group.Provider, group.SourceLabel, w.Window)
		}
		if !pulseable {
			t.Fatalf("monitor-only target=%+v", view)
		}
	}
	for _, provider := range []string{"claude", "kimi", "xai"} {
		cred := &Cred{ID: provider + ".json", AuthIndex: provider, Provider: provider}
		e.creds[provider] = cred
		label, id := "Fable", quota.WindowFable
		if provider != "claude" {
			label, id = "main", quota.WindowSevenDay
		}
		e.groups[provider+":monitor"] = &GroupView{Key: provider + ":monitor", Provider: provider, AuthIndex: provider, SourceLabel: label, Windows: []WindowView{{Window: quota.Window{ID: id, Period: quota.SevenDays}}}}
	}
	e.noteModels([]string{"claude-haiku-5-5", "kimi-k2", "grok-3-mini"}, nil)
	e.refreshTargets(cfg)
	for _, a := range e.Status().Accounts {
		for _, g := range a.Groups {
			if strings.HasSuffix(g.Key, ":monitor") {
				if g.AvailableModels != nil {
					t.Fatalf("monitor model choices=%+v", g)
				}
				raw, _ := json.Marshal(g)
				if strings.Contains(string(raw), "available_models") {
					t.Fatalf("monitor choices serialized: %s", raw)
				}
			}
		}
	}
	for _, view := range e.Status().Targets {
		if strings.HasSuffix(view.Key, ":monitor") {
			t.Fatal("monitor target published")
		}
	}
}

func TestPricesExcludeResellerAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"openai":{"models":{"gpt-5-nano":{"cost":{"input":1,"output":1},"modalities":{"output":["text"]}}}},"reseller":{"models":{"gpt-5-nano":{"cost":{"input":1,"output":1},"modalities":{"output":["text"]}}}}}`))
	}))
	defer server.Close()
	c := &clock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, testZone)}
	e, _ := newTestEngine(t, standardHost(c), c)
	e.pricingMgr = pricing.NewManager(t.TempDir(), pricing.WithURL(server.URL))
	if err := e.pricingMgr.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.noteModels([]string{"gpt-5-nano"}, nil)
	rows := e.Prices().Entries
	if len(rows) != 2 {
		t.Fatalf("rows=%v", rows)
	}
	for _, r := range rows {
		if r.Provider == "reseller" && (r.CPARelevant || r.AvailableInCPA || r.Eligible) {
			t.Fatalf("reseller availability leaked: %+v", r)
		}
		if r.Provider == "openai" && !r.AvailableInCPA {
			t.Fatal("native availability missing")
		}
	}
}

func TestNormalUsageClearsFailuresAfterAllEnabledWindowsStart(t *testing.T) {
	five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.SevenDays)
	e, _, c := windowEngine(t, []quota.Window{five, long}, nil)
	ts := e.state.Target("codex:1:codex:main")
	ts.ConsecutiveFailures = 3
	ts.RetryAtEpoch = float64(c.t.Add(time.Hour).Unix())
	ts.CircuitOpenUntilEpoch = ts.RetryAtEpoch
	five.Reset = c.t.Add(quota.FiveHours)
	apply := func() {
		e.applyGroups(context.Background(), e.config(), e.creds["1"], []quota.RawGroup{{Key: "codex:main", SourceLabel: "GPT", Windows: []quota.Window{five, long}}}, quota.SourcePassive, c.t, c.t, false)
	}
	apply()
	if ts.ConsecutiveFailures != 3 || ts.CircuitOpenUntilEpoch == 0 {
		t.Fatal("active short cleared pending long failures")
	}
	long.Reset = c.t.Add(quota.SevenDays)
	apply()
	if ts.ConsecutiveFailures != 0 || ts.CircuitOpenUntilEpoch != 0 || ts.RetryAtEpoch != 0 {
		t.Fatalf("normal usage retained failures: %+v", ts)
	}
}

func TestActiveObservationsKeepFailuresUntilFixed(t *testing.T) {
	for _, period := range []time.Duration{quota.FiveHours, quota.SevenDays, quota.ThirtyDays} {
		t.Run(period.String(), func(t *testing.T) {
			w := expiredPulseWindow(period)
			e, _, c := windowEngine(t, []quota.Window{w}, nil)
			ts := e.state.Target("codex:1:codex:main")
			ws := ts.Windows[w.ID]
			ws.ResetObservation = nil
			ts.ConsecutiveFailures = 3
			ts.RetryAtEpoch = float64(c.t.Add(time.Hour).Unix())
			ts.CircuitOpenUntilEpoch = ts.RetryAtEpoch
			apply := func() {
				e.applyGroups(context.Background(), e.config(), e.creds["1"], []quota.RawGroup{{Key: "codex:main", SourceLabel: "GPT", Windows: []quota.Window{w}}}, quota.SourceActive, c.t, c.t, false)
			}
			for _, step := range []struct {
				delay time.Duration
				moves bool
			}{
				{0, true}, {time.Second, true}, {time.Minute, true}, {time.Second, true}, {5 * time.Minute, false},
			} {
				c.add(step.delay)
				if step.moves {
					w.Reset = c.t.Add(period)
				}
				apply()
				if ts.ConsecutiveFailures != 3 || ts.RetryAtEpoch == 0 || ts.CircuitOpenUntilEpoch == 0 {
					t.Fatalf("unconfirmed observation cleared failures: %+v", ts)
				}
			}
			c.add(5 * time.Minute)
			apply()
			if ts.ConsecutiveFailures != 0 || ts.RetryAtEpoch != 0 || ts.CircuitOpenUntilEpoch != 0 {
				t.Fatalf("fixed observations retained failures: %+v", ts)
			}
		})
	}
}

func TestMonthlyMCPHistoryAndForecast(t *testing.T) {
	monthly := expiredPulseWindow(quota.ThirtyDays)
	e, _, c := windowEngine(t, []quota.Window{monthly}, nil)
	for _, offset := range []time.Duration{-time.Hour, 0} {
		if err := e.history.Append(store.Record{Kind: "s", Time: c.t.Add(offset).Unix(), Group: "codex:1:codex:main", Window: monthly.ID, Remaining: 50}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := NewMCPBackend(e).history(context.Background(), map[string]string{"window": "30d", "range": "31d"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), monthly.ID) {
		t.Fatalf("monthly MCP history missing: %s", raw)
	}
	for _, days := range []int{28, 30, 31} {
		period := time.Duration(days) * 24 * time.Hour
		w := WindowStatus{Kind: quota.KindThirtyDay, Remaining: 50, Reset: c.t.Add(period / 2), ObservedAt: c.t, PeriodSeconds: period.Seconds()}
		dto := forecastWindow(w, TargetView{}, nil, c.t, 5*time.Minute)
		want := 50 / (period.Hours() / 2)
		if dto.Rate == nil || math.Abs(*dto.Rate-want) > 1e-9 || dto.Started == nil || !*dto.Started {
			t.Fatalf("monthly forecast=%+v", dto)
		}
	}
}

func TestLongOnlySuccessEventIncludesReset(t *testing.T) {
	long := expiredPulseWindow(quota.ThirtyDays)
	e, _, _ := windowEngine(t, []quota.Window{long}, map[string]bool{long.ID: true})
	if !e.performDue(context.Background(), e.config()) {
		t.Fatal("no long pulse")
	}
	events := e.Status().Events
	for _, event := range events {
		if event.Kind == "ignite" {
			raw, _ := json.Marshal(event)
			if strings.Contains(string(raw), "unknown") || !strings.Contains(string(raw), "reset") {
				t.Fatalf("long success event=%s", raw)
			}
			return
		}
	}
	t.Fatal("missing ignition event")
}

func TestActiveUsageNeedsFixedWindowsBeforeClearingCircuit(t *testing.T) {
	five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.SevenDays)
	e, _, c := windowEngine(t, []quota.Window{five, long}, nil)
	ts := e.state.Target("codex:1:codex:main")
	ts.ConsecutiveFailures = 3
	ts.CircuitOpenUntilEpoch = float64(c.t.Add(time.Hour).Unix())
	apply := func() {
		e.applyGroups(context.Background(), e.config(), e.creds["1"], []quota.RawGroup{{Key: "codex:main", SourceLabel: "GPT", Windows: []quota.Window{five, long}}}, quota.SourceActive, c.t, c.t, false)
	}
	for i := 0; i < 3; i++ {
		c.add(time.Minute)
		five.Reset = c.t.Add(quota.FiveHours)
		long.Reset = c.t.Add(quota.SevenDays)
		apply()
		if ts.ConsecutiveFailures != 3 || ts.CircuitOpenUntilEpoch == 0 {
			t.Fatal("rolling placeholders cleared circuit")
		}
	}
	// Repeated observations of the same future resets establish fixed windows.
	c.add(time.Minute)
	apply()
	c.add(time.Minute)
	apply()
	if ts.ConsecutiveFailures != 0 || ts.CircuitOpenUntilEpoch != 0 {
		t.Fatalf("fixed observations retained circuit: %+v", ts)
	}
}
