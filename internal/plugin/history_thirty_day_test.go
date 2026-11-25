package plugin

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/config"
	"github.com/NotRllyRn/cpa-pulse/internal/engine"
	"github.com/NotRllyRn/cpa-pulse/internal/quota"
	"github.com/NotRllyRn/cpa-pulse/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestHistoryThirtyDayManagementRange(t *testing.T) {
	dir := t.TempDir()
	p := New(nopHost{}, "test", dir, "")
	defer p.Shutdown()
	now := time.Now().UTC()
	historyStore := &store.History{Dir: filepath.Join(dir, "history")}
	for _, id := range []string{quota.WindowFiveHour, quota.WindowSevenDay, quota.WindowThirtyDay} {
		if err := historyStore.Append(store.Record{Kind: "s", Time: now.Add(-time.Hour).Unix(), Group: "account:main", Window: id, Remaining: 80}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Ignition.Enabled = false
	cfg.Pricing.RefreshHours = 0
	p.engine.Configure(cfg, nil)
	p.engine.Start()
	// The management handler reads the same persisted history as the background engine.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for !p.engine.Status().Running {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("engine did not start")
		}
	}
	for _, key := range []string{"31d", ""} {
		request, _ := json.Marshal(pluginapi.ManagementRequest{Method: http.MethodGet, Path: apiBase + "/history", Query: map[string][]string{"window": {"30d"}, "range": {key}}})
		var response pluginapi.ManagementResponse
		if err := json.Unmarshal(result(t, p.Handle("management.handle", request)), &response); err != nil {
			t.Fatal(err)
		}
		var history engine.HistoryResponse
		if err := json.Unmarshal(response.Body, &history); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK || history.To.Sub(history.From) != 31*24*time.Hour || len(history.Series) != 1 || history.Series[0].Kind != quota.KindThirtyDay || history.Series[0].Window != quota.WindowThirtyDay || history.Events == nil {
			t.Fatalf("range=%q status=%d body=%s", key, response.StatusCode, response.Body)
		}
	}
}
