package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/config"
	"github.com/NotRllyRn/cpa-pulse/internal/host"
	"github.com/NotRllyRn/cpa-pulse/internal/pricing"
	"github.com/NotRllyRn/cpa-pulse/internal/providers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func pricingTarget(t *testing.T, e *Engine, cfg config.Config) target {
	t.Helper()
	e.poll(context.Background(), cfg, e.now(), "", false)
	for _, candidate := range e.collectTargets(cfg) {
		if candidate.group.Provider == "claude" {
			return candidate
		}
	}
	t.Fatal("missing Claude target")
	return target{}
}

func serveModels(t *testing.T, cfg *config.Config, ids []string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data := []map[string]string{}
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	cfg.CPABaseURL = server.URL
}

func TestCheapestSelectionAndAccountOverride(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, testZone)}
	h := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, h, c)
	cfg := e.config()
	ids := []string{"claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5", "claude-opus-5-5", "gpt-5-nano"}
	serveModels(t, &cfg, ids)
	chosen := pricingTarget(t, e, cfg)
	// A cheap Opus must remain eligible even though the provider defaults prefer Haiku.
	if err := e.SetPriceOverride("claude", "claude-opus-5-5", pricing.Override{Price: &pricing.Price{InputUSDPerMillion: 0.01, OutputUSDPerMillion: 0.01}}); err != nil {
		t.Fatal(err)
	}
	model, _, err := e.execute(context.Background(), cfg, chosen)
	if err != nil || model != "claude-opus-5-5" {
		t.Fatalf("cheapest: %s %v", model, err)
	}
	cfg.Providers["claude"] = config.Provider{Monitor: true, Ignite: true, Model: "claude-haiku-5-5", Models: map[string]string{"Claude": "claude-sonnet-5-5"}}
	model, _, err = e.execute(context.Background(), cfg, chosen)
	if err != nil || model != "claude-sonnet-5-5" {
		t.Fatalf("provider/group: %s %v", model, err)
	}
	cfg.AccountModels = map[string]map[string]string{chosen.cred.ID: {chosen.group.Key: "claude-opus-5-5"}}
	model, _, err = e.execute(context.Background(), cfg, chosen)
	if err != nil || model != "claude-opus-5-5" {
		t.Fatalf("account/group: %s %v", model, err)
	}
	other := chosen
	other.cred = &Cred{ID: "other.json", AuthIndex: "other", Provider: "claude"}
	other.group.Key = "claude:other:claude:main"
	other.group.AuthIndex = "other"
	model, _, err = e.execute(context.Background(), cfg, other)
	if err != nil || model != "claude-sonnet-5-5" {
		t.Fatalf("other account must retain provider override: %s %v", model, err)
	}
	e.Configure(cfg, nil)
	e.noteModels(ids, nil)
	gs := GroupStatus{}
	authID := ""
	for _, account := range e.Status().Accounts {
		if account.AuthIndex == chosen.cred.AuthIndex {
			authID = account.AuthID
			gs = account.Groups[0]
		}
	}
	if authID != chosen.cred.ID || gs.ManualModel != "claude-opus-5-5" || gs.EstimatedCheapestModel != "claude-opus-5-5" || gs.EstimatedCheapestCostUSD <= 0 || len(gs.AvailableModels) != 3 {
		t.Fatalf("status auth=%s group=%+v", authID, gs)
	}
}

func TestConfirmedModelBreaksPriceTiesPerAccount(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, testZone)}
	h := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, h, c)
	cfg := e.config()
	ids := []string{"claude-haiku-5-5", "claude-sonnet-5-5"}
	serveModels(t, &cfg, ids)
	chosen := pricingTarget(t, e, cfg)
	for _, id := range ids {
		if err := e.SetPriceOverride("claude", id, pricing.Override{Price: &pricing.Price{InputUSDPerMillion: 1, OutputUSDPerMillion: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	e.state.Target(chosen.group.Key).ConfirmedModel = ids[1]
	model, _, err := e.execute(context.Background(), cfg, chosen)
	if err != nil || model != ids[1] {
		t.Fatalf("confirmed tie: %s %v", model, err)
	}
	other := chosen
	other.cred = &Cred{ID: "other.json", AuthIndex: "other", Provider: "claude"}
	other.group.Key = "claude:other:claude:main"
	other.group.AuthIndex = "other"
	model, _, err = e.execute(context.Background(), cfg, other)
	if err != nil || model != ids[0] {
		t.Fatalf("confirmation leaked accounts: %s %v", model, err)
	}
	cfg.AccountModels = map[string]map[string]string{chosen.cred.ID: {"other-group": ids[1]}}
	if manualModel(cfg, chosen) != "" {
		t.Fatal("override leaked across groups")
	}
}

func TestStrictManualModelNeverFallsBack(t *testing.T) {
	for _, failure := range []error{
		&host.Error{Status: 404, Code: "model_not_found"},
		&host.Error{Status: 503, Code: "auth_not_found"},
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, testZone)}
			h := claudeIgnitionHost(c, failure)
			e, _ := newTestEngine(t, h, c)
			cfg := e.config()
			chosen := pricingTarget(t, e, cfg)
			cfg.AccountModels = map[string]map[string]string{chosen.cred.ID: {chosen.group.Key: "claude-haiku-4-5-20251001"}}
			_, _, err := e.execute(context.Background(), cfg, chosen)
			if err == nil || len(h.executed) != 1 || h.executed[0].Model != "claude-haiku-4-5-20251001" {
				t.Fatalf("manual attempts=%v err=%v", h.executed, err)
			}
			cfg.AccountModels[chosen.cred.ID][chosen.group.Key] = "missing"
			_, _, err = e.execute(context.Background(), cfg, chosen)
			if err == nil || len(h.executed) != 1 {
				t.Fatalf("missing manual model must stop: %v", err)
			}
		})
	}
}

func TestModelFallbackGuardrails(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		attempts int
	}{
		{"model 404", &host.Error{Status: 404, Code: "model_not_found"}, 2},
		{"model 403", &host.Error{Status: 403, Message: "You do not have access to model claude-haiku-5-5"}, 2},
		{"model 400", &host.Error{Status: 400, Code: "model_not_supported"}, 2},
		{"generic 404", &host.Error{Status: 404, Code: "model_execution_failed", Message: "route not found"}, 1},
		{"generic 403", &host.Error{Status: 403, Message: "permission denied"}, 1},
		{"generic 400", &host.Error{Status: 400, Code: "invalid_request_error"}, 1},
		{"auth", &host.Error{Status: 401, Code: "model_not_found"}, 1},
		{"rate limit", &host.Error{Status: 429, Code: "model_not_supported"}, 1},
		{"suspension", &host.Error{Status: 403, Message: "account has been suspended; model not available"}, 1},
		{"network", errors.New("network down"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, testZone)}
			h := claudeIgnitionHost(c, nil)
			h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
				return pluginapi.HostModelExecutionResponse{}, tc.err
			}
			e, _ := newTestEngine(t, h, c)
			cfg := e.config()
			serveModels(t, &cfg, []string{"claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5"})
			chosen := pricingTarget(t, e, cfg)
			_, _, err := e.execute(context.Background(), cfg, chosen)
			if err == nil || len(h.executed) != tc.attempts {
				t.Fatalf("attempts=%d want=%d err=%v", len(h.executed), tc.attempts, err)
			}
		})
	}
}

func TestLocalRoutingSkipsAndTotalAttemptCap(t *testing.T) {
	for _, localCount := range []int{4, 20} {
		t.Run(fmt.Sprint(localCount), func(t *testing.T) {
			c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, testZone)}
			h := claudeIgnitionHost(c, nil)
			calls := 0
			h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
				calls++
				if calls <= localCount {
					return pluginapi.HostModelExecutionResponse{}, &host.Error{Status: 503, Code: "auth_not_found"}
				}
				return pluginapi.HostModelExecutionResponse{}, &host.Error{Status: 404, Code: "model_not_found"}
			}
			e, _ := newTestEngine(t, h, c)
			cfg := e.config()
			ids := []string{}
			for i := 0; i < 10; i++ {
				ids = append(ids, fmt.Sprintf("claude-custom-%d", i))
			}
			serveModels(t, &cfg, ids)
			chosen := pricingTarget(t, e, cfg)
			_, _, err := e.execute(context.Background(), cfg, chosen)
			if err == nil || calls != 6 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if localCount == 4 && len(e.refused) != 2 {
				t.Fatalf("upstream attempts were counted incorrectly: %v", e.refused)
			}
		})
	}
}

func TestRefusalsPersistExpireAndClearWithCredentials(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, testZone)}
	h := claudeIgnitionHost(c, nil)
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, &host.Error{Status: 404, Code: "model_not_found"}
	}
	dir := t.TempDir()
	e, _ := newTestEngineIn(t, h, c, dir)
	cfg := e.config()
	serveModels(t, &cfg, []string{"claude-haiku-5-5", "claude-sonnet-5-5"})
	chosen := pricingTarget(t, e, cfg)
	_, _, _ = e.execute(context.Background(), cfg, chosen)
	e.saveState()
	if len(e.refused) != 2 {
		t.Fatalf("refusals=%v", e.refused)
	}
	restarted, _ := newTestEngineIn(t, h, c, dir)
	before := len(h.executed)
	_, _, err := restarted.execute(context.Background(), cfg, chosen)
	if err == nil || !strings.Contains(err.Error(), "no eligible model; retry after cooldown") || len(h.executed) != before {
		t.Fatalf("cooldown err=%v attempts=%d", err, len(h.executed)-before)
	}
	other := chosen
	other.cred = &Cred{ID: "other.json", AuthIndex: "other", Provider: "claude"}
	other.group.Key = "claude:other:claude:main"
	other.group.AuthIndex = "other"
	_, _, _ = restarted.execute(context.Background(), cfg, other)
	if len(h.executed) != before+2 {
		t.Fatal("refusals leaked across accounts")
	}
	c.add(refusedFor - time.Second)
	_, _, err = restarted.execute(context.Background(), cfg, chosen)
	if err == nil || !strings.Contains(err.Error(), "retry after cooldown") {
		t.Fatalf("TTL before expiry: %v", err)
	}
	c.add(time.Second)
	before = len(h.executed)
	_, _, _ = restarted.execute(context.Background(), cfg, chosen)
	if len(h.executed) != before+2 {
		t.Fatal("expired refusals did not allow retry")
	}
	restarted.state.Target(chosen.group.Key).ConfirmedModel = "claude-haiku-5-5"
	h.files[chosen.cred.AuthIndex] = `{"access_token":"new-login"}`
	restarted.noteFingerprint(chosen.cred, providers.Fingerprint(json.RawMessage(h.files[chosen.cred.AuthIndex])))
	if len(restarted.skipRefused(chosen.group.Key, []string{"claude-haiku-5-5", "claude-sonnet-5-5"})) != 2 || restarted.state.Target(chosen.group.Key).ConfirmedModel != "" {
		t.Fatal("credential change retained model access")
	}
}

func TestRankingFallbackAndTombstones(t *testing.T) {
	mgr := pricing.NewManager(t.TempDir())
	spec := providers.Lookup("claude")
	ids := []string{"claude-custom-1", "claude-custom-2", "claude-opus-5-5"}
	if got := rankedModels(mgr, spec, "Claude", ids, ""); !reflect.DeepEqual(got, []string{"claude-opus-5-5"}) {
		t.Fatalf("priced eligibility: %v", got)
	}
	if err := mgr.SetOverride("claude", "claude-opus-5-5", pricing.Override{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := rankedModels(mgr, spec, "Claude", ids, ""); !reflect.DeepEqual(got, ids[:2]) {
		t.Fatalf("default fallback must exclude disabled: %v", got)
	}
}

func TestPriceRefreshIsAsyncSingleFlightAndManualOnly(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, testZone)}
	h := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, h, c)
	e.pricingMgr = pricing.NewManager(t.TempDir(), pricing.WithURL(server.URL))
	cfg := e.config()
	cfg.Pricing.RefreshHours = 0
	e.maybeRefreshPrices(context.Background(), cfg, c.t)
	if e.Prices().Refreshing || calls.Load() != 0 {
		t.Fatal("manual-only scheduled a refresh")
	}
	chosen := pricingTarget(t, e, cfg)
	if _, _, err := e.execute(context.Background(), cfg, chosen); err != nil || calls.Load() != 0 {
		t.Fatalf("pulse fetched pricing: %v calls=%d", err, calls.Load())
	}
	cfg.Pricing.RefreshHours = 24
	e.maybeRefreshPrices(context.Background(), cfg, c.t)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	for i := 0; i < 10; i++ {
		e.RefreshPrices()
	}
	if !e.Prices().Refreshing || calls.Load() != 1 {
		t.Fatalf("single flight: %+v calls=%d", e.Prices(), calls.Load())
	}
	close(release)
	deadline := time.After(time.Second)
	for e.Prices().Refreshing {
		select {
		case <-e.wakeCh:
		case <-deadline:
			t.Fatal("refresh did not finish")
		}
	}
	if e.Prices().LastError == "" || calls.Load() != 1 {
		t.Fatal("refresh error was not reported")
	}
}

func TestSuccessfulModelStatusSurvivesRestart(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, testZone)}
	h := claudeIgnitionHost(c, nil)
	dir := t.TempDir()
	e, _ := newTestEngineIn(t, h, c, dir)
	chosen := pricingTarget(t, e, e.config())
	if err := e.igniteTarget(context.Background(), e.config(), chosen, false); err != nil {
		t.Fatal(err)
	}
	e.saveState()
	restarted, _ := newTestEngineIn(t, h, c, dir)
	ts := restarted.state.Target(chosen.group.Key)
	if ts.LastSuccessfulModel == "" || ts.ConfirmedModel != ts.LastSuccessfulModel {
		t.Fatalf("state=%+v", ts)
	}
	pricingTarget(t, restarted, restarted.config())
	for _, account := range restarted.Status().Accounts {
		if account.AuthIndex == chosen.cred.AuthIndex {
			if len(account.Groups) != 1 || account.Groups[0].LastSuccessfulModel != ts.LastSuccessfulModel || account.Groups[0].ConfirmedModel != ts.ConfirmedModel {
				t.Fatalf("account=%+v", account)
			}
			return
		}
	}
	t.Fatal("missing status account")
}

func TestPriceStatusEligibilityAndAliases(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, testZone)}
	e, _ := newTestEngine(t, claudeIgnitionHost(c, nil), c)
	e.noteModels([]string{"claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", "gpt-5-nano"}, nil)
	if err := e.SetPriceOverride("codex", "gpt-5-nano", pricing.Override{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	prices := e.Prices()
	if len(prices.AvailableModels) != 2 {
		t.Fatalf("available models were not deduplicated: %v", prices.AvailableModels)
	}
	var alias, disabled bool
	for _, row := range prices.Entries {
		if row.Provider == "anthropic" && row.Model == "claude-haiku-4-5" {
			alias = row.AvailableInCPA && row.CPARelevant && row.Eligible && row.EstimatedPulseUSD > 0
		}
		if row.Provider == "openai" && row.Model == "gpt-5-nano" {
			disabled = row.Disabled && !row.Eligible && row.AvailableInCPA && row.EstimatedPulseUSD > 0 && row.Reason == "Disabled by user"
		}
	}
	if !alias || !disabled {
		t.Fatalf("alias=%v disabled=%v", alias, disabled)
	}
}

func TestStreamModelEntitlementMetadataIsSafe(t *testing.T) {
	explicit := streamErrorText(`{"error":{"status":403,"type":"permission_error","message":"You do not have access to model SECRET"}}`)
	generic := streamErrorText(`{"error":{"status":403,"type":"permission_error","message":"permission denied SECRET"}}`)
	if !modelRejected(explicit) || modelRejected(generic) || strings.Contains(explicit.Error()+generic.Error(), "SECRET") {
		t.Fatalf("explicit=%v generic=%v", explicit, generic)
	}
}
