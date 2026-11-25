package engine

import (
	"context"
	"strings"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/config"
	"github.com/NotRllyRn/cpa-pulse/internal/pricing"
	"github.com/NotRllyRn/cpa-pulse/internal/providers"
)

type modelState struct {
	LastSuccessfulModel string
	ConfirmedModel      string
	Refused             map[string]time.Time
}

func (e *Engine) priceManager() *pricing.Manager {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pricingMgr
}

// rankedModels uses default provider ordering only when no compatible model has a usable price.
func rankedModels(mgr *pricing.Manager, spec *providers.Provider, group string, available []string, confirmed string) []string {
	defaults, _ := providers.Candidates(spec, group, "", available)
	if spec == nil {
		return nil
	}
	if mgr == nil {
		return defaults
	}
	ranked := mgr.Rank(spec.ID, defaults, confirmed)
	out := []string{}
	for _, candidate := range ranked {
		if candidate.Eligible {
			out = append(out, candidate.ModelID)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, model := range defaults {
		entry, found := mgr.Resolve(spec.ID, model)
		if found && (entry.Disabled || entry.Deprecated || !entry.TextGeneration) {
			continue
		}
		out = append(out, model)
	}
	return out
}

func manualModel(cfg config.Config, t target) string {
	if t.cred != nil {
		if model := strings.TrimSpace(cfg.AccountModels[t.cred.ID][t.group.Key]); model != "" {
			return model
		}
	}
	return configuredModel(cfg, t.group.Provider, t.group.SourceLabel)
}

func (e *Engine) publishModelState(key string) {
	if e.state == nil {
		return
	}
	ts := e.state.Target(key)
	view := modelState{LastSuccessfulModel: ts.LastSuccessfulModel, ConfirmedModel: ts.ConfirmedModel, Refused: map[string]time.Time{}}
	for k, at := range e.refused {
		if model, found := strings.CutPrefix(k, key+"\x00"); found && e.now().Sub(at) < refusedFor {
			view.Refused[model] = at
		}
	}
	e.mu.Lock()
	e.modelStates[key] = view
	e.mu.Unlock()
}

func (e *Engine) maybeRefreshPrices(ctx context.Context, cfg config.Config, now time.Time) {
	if e.priceManager().NeedsRefresh(now, cfg.Pricing.RefreshHours) {
		e.startPriceRefresh(ctx)
	}
}

// RefreshPrices starts a background refresh; concurrent requests share one fetch.
func (e *Engine) RefreshPrices() { e.startPriceRefresh(context.Background()) }

func (e *Engine) startPriceRefresh(parent context.Context) {
	e.mu.Lock()
	if e.pricingRefreshing || e.stopped {
		e.mu.Unlock()
		return
	}
	e.pricingRefreshing = true
	mgr := e.pricingMgr
	e.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		done := make(chan struct{})
		go func() {
			select {
			case <-e.stopCh:
				cancel()
			case <-done:
			}
		}()
		_ = mgr.Refresh(ctx)
		close(done)
		cancel()
		e.mu.Lock()
		e.pricingRefreshing = false
		e.mu.Unlock()
		e.wake()
	}()
}

type PriceEntry struct {
	Provider            string  `json:"provider"`
	Model               string  `json:"model"`
	InputUSDPerMillion  float64 `json:"input_usd_per_million"`
	OutputUSDPerMillion float64 `json:"output_usd_per_million"`
	HasPrice            bool    `json:"has_price"`
	EstimatedPulseUSD   float64 `json:"estimated_pulse_usd"`
	TextGeneration      bool    `json:"text_generation"`
	Deprecated          bool    `json:"deprecated"`
	Source              string  `json:"source"`
	Custom              bool    `json:"custom"`
	Disabled            bool    `json:"disabled"`
	CPARelevant         bool    `json:"cpa_relevant"`
	AvailableInCPA      bool    `json:"available_in_cpa"`
	Eligible            bool    `json:"eligible"`
	Reason              string  `json:"reason"`
}

type PricesStatus struct {
	Source          string       `json:"source"`
	FetchedAt       time.Time    `json:"fetched_at"`
	CheckedAt       time.Time    `json:"checked_at"`
	RefreshHours    int          `json:"refresh_hours"`
	Refreshing      bool         `json:"refreshing"`
	LastError       string       `json:"last_error"`
	AvailableModels []string     `json:"available_models"`
	Entries         []PriceEntry `json:"entries"`
}

// Prices returns local catalog data without fetching prices or CPA models.
func (e *Engine) Prices() PricesStatus {
	e.mu.Lock()
	mgr := e.pricingMgr
	out := PricesStatus{RefreshHours: e.cfg.Pricing.RefreshHours, Refreshing: e.pricingRefreshing, AvailableModels: append([]string{}, e.availableModels...), Entries: []PriceEntry{}}
	e.mu.Unlock()
	snapshot := mgr.Snapshot()
	out.Source, out.FetchedAt, out.CheckedAt, out.LastError = snapshot.Source, snapshot.FetchedAt, snapshot.CheckedAt, snapshot.LastError
	resolved := map[string]bool{}
	for _, provider := range []string{"anthropic", "openai", "google", "moonshotai", "xai", "antigravity"} {
		for _, candidate := range mgr.Rank(provider, out.AvailableModels, "") {
			if candidate.CatalogModel != "" {
				resolved[candidate.CatalogProvider+"\x00"+candidate.CatalogModel] = true
			}
		}
	}
	for _, entry := range mgr.Catalog() {
		row := PriceEntry{
			Provider: entry.Provider, Model: entry.Model, InputUSDPerMillion: entry.Price.InputUSDPerMillion, OutputUSDPerMillion: entry.Price.OutputUSDPerMillion,
			HasPrice: entry.HasPrice, TextGeneration: entry.TextGeneration, Deprecated: entry.Deprecated,
			Source: entry.Source, Custom: entry.Custom, Disabled: entry.Disabled, CPARelevant: pricing.IsCPAProvider(entry.Provider),
			AvailableInCPA: pricing.IsCPAProvider(entry.Provider) && resolved[entry.Provider+"\x00"+entry.Model],
		}
		if row.HasPrice {
			row.EstimatedPulseUSD = entry.Price.EstimatedPulseUSD()
		}
		switch {
		case !row.CPARelevant:
			row.Reason = "Provider is not used by CPA"
		case !row.AvailableInCPA:
			row.Reason = "Model is not available in CPA"
		case row.Disabled:
			row.Reason = "Disabled by user"
		case row.Deprecated:
			row.Reason = "Model is deprecated or retired"
		case !row.TextGeneration:
			row.Reason = "Model does not support text generation"
		case !row.HasPrice:
			row.Reason = "Input or output price is missing"
		default:
			row.Eligible = true
		}
		out.Entries = append(out.Entries, row)
	}
	return out
}

func (e *Engine) SetPriceOverride(provider, model string, override pricing.Override) error {
	return e.priceManager().SetOverride(provider, model, override)
}

func (e *Engine) DeletePriceOverride(provider, model string) error {
	return e.priceManager().DeleteOverride(provider, model)
}
