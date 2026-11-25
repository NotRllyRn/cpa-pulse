package pricing

import (
	"sort"
	"strings"
)

// IsCPAProvider identifies native catalog families used by CPA accounts. The
// catalog itself retains all providers for explicit searches.
func IsCPAProvider(provider string) bool {
	switch strings.ToLower(provider) {
	case "openai", "codex", "anthropic", "claude", "google", "gemini", "antigravity", "xai", "x-ai", "grok", "moonshotai", "moonshot", "kimi":
		return true
	default:
		return false
	}
}

func catalogProvider(provider, model string) string {
	normalizedModel := strings.ToLower(model)
	switch strings.ToLower(provider) {
	case "codex", "openai":
		return "openai"
	case "claude", "anthropic":
		return "anthropic"
	case "google", "gemini":
		return "google"
	case "antigravity":
		if strings.HasPrefix(normalizedModel, "claude-") {
			return "anthropic"
		}
		if strings.HasPrefix(normalizedModel, "gemini-") {
			return "google"
		}
		if strings.HasPrefix(normalizedModel, "gpt-") {
			return "openai"
		}
		return "antigravity"
	case "kimi", "moonshot", "moonshotai":
		return "moonshotai"
	case "xai", "x-ai", "grok":
		return "xai"
	default:
		return provider
	}
}

// Alias groups are explicit equivalents. Exact catalog IDs always win.
var modelAliases = map[string][][]string{
	"anthropic": {
		{"claude-3-5-haiku-20241022", "claude-3-5-haiku-latest"},
		{"claude-3-5-sonnet-20241022", "claude-3-5-sonnet-latest"},
		{"claude-3-7-sonnet-20250219", "claude-3-7-sonnet-latest"},
		{"claude-sonnet-4", "claude-sonnet-4-20250514"},
		{"claude-opus-4", "claude-opus-4-20250514"},
		{"claude-opus-4-1", "claude-opus-4-1-20250805"},
		{"claude-sonnet-4-5", "claude-sonnet-4-5-20250929"},
		{"claude-haiku-4-5", "claude-haiku-4-5-20251001"},
		{"claude-opus-4-5", "claude-opus-4-5-20251101"},
	},
	"openai": {
		{"gpt-4o-mini", "gpt-4o-mini-2024-07-18"},
		{"gpt-4.1", "gpt-4.1-2025-04-14"},
		{"gpt-4.1-mini", "gpt-4.1-mini-2025-04-14"},
		{"gpt-4.1-nano", "gpt-4.1-nano-2025-04-14"},
		{"gpt-5", "gpt-5-2025-08-07"},
		{"gpt-5-mini", "gpt-5-mini-2025-08-07"},
		{"gpt-5-nano", "gpt-5-nano-2025-08-07"},
		{"o4-mini", "o4-mini-2025-04-16"},
	},
}

func canonicalModel(provider, model string) string {
	for _, group := range modelAliases[provider] {
		for _, alias := range group {
			if alias == model {
				return group[0]
			}
		}
	}
	return model
}

func resolveEntries(byID map[string]Entry, provider, model string) (Entry, bool) {
	provider = catalogProvider(provider, model)
	if e, ok := byID[provider+"/"+model]; ok {
		return e, true
	}
	for _, group := range modelAliases[provider] {
		match := false
		for _, alias := range group {
			if alias == model {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		for _, alias := range group {
			if e, ok := byID[provider+"/"+alias]; ok {
				return e, true
			}
		}
	}
	return Entry{}, false
}

// Resolve uses the account's native provider; it never selects a reseller.
func (m *Manager) Resolve(cpaProvider, modelID string) (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return resolveEntries(m.lookup, cpaProvider, modelID)
}

// Rank returns eligible models first, followed by excluded models with reasons.
func (m *Manager) Rank(cpaProvider string, candidateIDs []string, confirmedModel string) []RankedCandidate {
	m.mu.RLock()
	entries := m.lookup
	m.mu.RUnlock()
	seen := make(map[string]bool, len(candidateIDs))
	ranked := make([]RankedCandidate, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		candidate := RankedCandidate{ModelID: id, Provider: cpaProvider, Confirmed: id == confirmedModel}
		e, ok := resolveEntries(entries, cpaProvider, id)
		if !ok {
			candidate.Reason = "No matching catalog model"
		} else {
			candidate.CatalogProvider = e.Provider
			candidate.CatalogModel = e.Model
			candidate.Price = e.Price
			candidate.Source = e.Source
			switch {
			case e.Disabled:
				candidate.Reason = "Disabled by user"
			case e.Deprecated:
				candidate.Reason = "Model is deprecated or retired"
			case !e.TextGeneration:
				candidate.Reason = "Model does not support text generation"
			case !e.HasPrice:
				candidate.Reason = "Input or output price is missing"
			default:
				candidate.Eligible = true
				candidate.EstimatedPulseUSD = e.Price.EstimatedPulseUSD()
			}
		}
		ranked = append(ranked, candidate)
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		if a.EstimatedPulseUSD != b.EstimatedPulseUSD {
			return a.EstimatedPulseUSD < b.EstimatedPulseUSD
		}
		if a.Confirmed != b.Confirmed {
			return a.Confirmed
		}
		return a.ModelID < b.ModelID
	})
	return ranked
}

func (m *Manager) Cheapest(cpaProvider string, candidateIDs []string, confirmedModel string) (RankedCandidate, bool) {
	ranked := m.Rank(cpaProvider, candidateIDs, confirmedModel)
	if len(ranked) == 0 || !ranked[0].Eligible {
		return RankedCandidate{}, false
	}
	return ranked[0], true
}
