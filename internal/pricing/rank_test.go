package pricing

import (
	"math"
	"reflect"
	"testing"
)

func TestExactAliasesAndNoFuzzyMatching(t *testing.T) {
	m := NewManager(t.TempDir())
	for _, test := range []struct{ provider, id, wantProvider, wantModel string }{
		{"codex", "gpt-4o-mini-2024-07-18", "openai", "gpt-4o-mini"},
		{"claude", "claude-sonnet-4-5-20250929", "anthropic", "claude-sonnet-4-5"},
		{"antigravity", "claude-haiku-4-5-20251001", "anthropic", "claude-haiku-4-5"},
		{"antigravity", "gemini-2.5-flash", "google", "gemini-2.5-flash"},
		{"gemini", "gemini-2.5-pro", "google", "gemini-2.5-pro"},
		{"kimi", "kimi-k2", "moonshotai", "kimi-k2"},
		{"xAI", "grok-3-mini", "xai", "grok-3-mini"},
	} {
		e, ok := m.Resolve(test.provider, test.id)
		if !ok || e.Provider != test.wantProvider || e.Model != test.wantModel {
			t.Errorf("%+v -> %+v, %v", test, e, ok)
		}
	}
	for _, test := range [][2]string{
		{"codex", "gpt-4o-mini-2099-01-01"}, {"claude", "claude-sonnet-4-5-thinking"},
		{"claude", "claude-sonnet-4-5-20990101"}, {"openai", "GPT-4o-mini"},
		{"antigravity", "unknown-model"}, {"unrelated", "gpt-4o-mini"},
	} {
		if e, ok := m.Resolve(test[0], test[1]); ok {
			t.Errorf("fuzzy match %v -> %+v", test, e)
		}
	}
	m.snapshot.Entries = append(m.snapshot.Entries, Entry{Provider: "reseller", Model: "exclusive", HasPrice: true, TextGeneration: true})
	m.rebuildCatalogLocked()
	if _, ok := m.Resolve("openai", "exclusive"); ok {
		t.Fatal("reseller selected")
	}
	if _, ok := m.Resolve("reseller", "exclusive"); !ok {
		t.Fatal("explicit provider not supported")
	}
	// Exact IDs take precedence even when an alias is present in the same catalog.
	m.snapshot.Entries = append(m.snapshot.Entries, Entry{Provider: "openai", Model: "gpt-4o-mini-2024-07-18", HasPrice: true, TextGeneration: true, Price: Price{9, 9}})
	m.rebuildCatalogLocked()
	if e, _ := m.Resolve("codex", "gpt-4o-mini-2024-07-18"); e.Price.InputUSDPerMillion != 9 {
		t.Fatal("alias beat exact ID")
	}
}

func TestRankMicroPulseAndReasons(t *testing.T) {
	m := NewManager(t.TempDir())
	m.snapshot.Entries = []Entry{
		{Provider: "openai", Model: "a", Price: Price{1, 1}, HasPrice: true, TextGeneration: true, Source: "models.dev"},
		{Provider: "openai", Model: "b", Price: Price{1, 1}, HasPrice: true, TextGeneration: true},
		{Provider: "openai", Model: "c", Price: Price{1, 1}, HasPrice: true, TextGeneration: true},
		{Provider: "openai", Model: "free", HasPrice: true, TextGeneration: true},
		{Provider: "openai", Model: "output-heavy", Price: Price{0, 20}, HasPrice: true, TextGeneration: true},
		{Provider: "openai", Model: "deprecated", HasPrice: true, TextGeneration: true, Deprecated: true},
		{Provider: "openai", Model: "nontext", HasPrice: true},
		{Provider: "openai", Model: "unpriced", TextGeneration: true},
		{Provider: "openai", Model: "disabled", HasPrice: true, TextGeneration: true},
	}
	if err := m.SetOverride("openai", "disabled", Override{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	ids := []string{"unknown", "unpriced", "disabled", "deprecated", "nontext", "output-heavy", "c", "a", "b", "free", "a"}
	got := m.Rank("codex", ids, "b")
	names := make([]string, 0, len(got))
	for _, c := range got {
		names = append(names, c.ModelID)
	}
	want := []string{"free", "b", "a", "c", "output-heavy", "deprecated", "disabled", "nontext", "unknown", "unpriced"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("order %v want %v", names, want)
	}
	if math.Abs(got[1].EstimatedPulseUSD-0.000034) > 1e-15 {
		t.Fatal(got[1].EstimatedPulseUSD)
	}
	if got[1].CatalogProvider != "openai" || got[1].Provider != "codex" || !got[1].Confirmed {
		t.Fatalf("metadata %+v", got[1])
	}
	for _, candidate := range got[5:] {
		if candidate.Eligible || candidate.Reason == "" {
			t.Errorf("missing exclusion reason %+v", candidate)
		}
	}
	cheapest, ok := m.Cheapest("codex", ids, "b")
	if !ok || cheapest.ModelID != "free" {
		t.Fatalf("wrong cheapest %+v", cheapest)
	}
	cheapest, ok = m.Cheapest("codex", []string{"b", "a"}, "b")
	if !ok || cheapest.ModelID != "b" {
		t.Fatal("confirmed tie-break lost")
	}
	for _, ids := range [][]string{nil, {"unknown", "disabled", "unpriced"}} {
		if _, ok := m.Cheapest("codex", ids, ""); ok {
			t.Fatal("excluded model selected")
		}
	}
}

func TestCPAProviderFilter(t *testing.T) {
	for _, p := range []string{"openai", "claude", "anthropic", "google", "antigravity", "moonshotai", "kimi", "xai", "codex"} {
		if !IsCPAProvider(p) {
			t.Error(p)
		}
	}
	if IsCPAProvider("reseller") {
		t.Fatal("reseller marked relevant")
	}
}
