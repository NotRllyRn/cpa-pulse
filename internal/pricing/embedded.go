package pricing

// EmbeddedEntries returns baseline USD prices per million tokens for offline use.
// A fetched catalog replaces this table; overrides take precedence over both.
func EmbeddedEntries() []Entry {
	models := []struct {
		provider, model string
		input, output   float64
	}{
		{"anthropic", "claude-3-5-haiku-20241022", 0.8, 4},
		{"anthropic", "claude-haiku-5-5", 0.1, 0.5},
		{"anthropic", "claude-sonnet-5-5", 2, 10},
		{"anthropic", "claude-opus-5-5", 4, 20},
		{"anthropic", "claude-haiku-4-5", 1, 5},
		{"anthropic", "claude-sonnet-4", 3, 15},
		{"anthropic", "claude-sonnet-4-5", 3, 15},
		{"anthropic", "claude-opus-4", 15, 75},
		{"anthropic", "claude-opus-4-5", 5, 25},
		{"openai", "gpt-4o-mini", 0.15, 0.6},
		{"openai", "gpt-5.4-mini", 0.75, 4.5},
		{"openai", "gpt-5.6-luna", 0.2, 1.2},
		{"openai", "gpt-6.1-terra", 1, 5},
		{"openai", "gpt-4o", 2.5, 10},
		{"openai", "gpt-4.1", 2, 8},
		{"openai", "gpt-4.1-mini", 0.4, 1.6},
		{"openai", "gpt-4.1-nano", 0.1, 0.4},
		{"openai", "gpt-5", 1.25, 10},
		{"openai", "gpt-5-mini", 0.25, 2},
		{"openai", "gpt-5-nano", 0.05, 0.4},
		{"openai", "o4-mini", 1.1, 4.4},
		{"google", "gemini-2.0-flash", 0.1, 0.4},
		{"google", "gemini-3.8-flash", 0.75, 3.75},
		{"google", "gemini-4-argon", 2, 10},
		{"google", "gemini-4-neon", 3, 15},
		{"google", "gemini-2.0-flash-lite", 0.075, 0.3},
		{"google", "gemini-2.5-flash", 0.3, 2.5},
		{"google", "gemini-2.5-flash-lite", 0.1, 0.4},
		{"google", "gemini-2.5-pro", 1.25, 10},
		{"google", "gemini-3-pro-preview", 2, 12},
		{"xai", "grok-3", 3, 15},
		{"xai", "grok-4.7", 2, 6},
		{"xai", "grok-3-mini", 0.3, 0.5},
		{"xai", "grok-4", 3, 15},
		{"xai", "grok-4-fast", 0.2, 0.5},
		{"moonshotai", "kimi-k2", 0.6, 2.5},
		{"moonshotai", "kimi-k2.6", 0.95, 4},
		{"moonshotai", "kimi-k3", 3, 15},
		{"moonshotai", "kimi-k2-thinking", 0.6, 2.5},
	}
	entries := make([]Entry, 0, len(models))
	for _, m := range models {
		entries = append(entries, Entry{Provider: m.provider, Model: m.model, Price: Price{m.input, m.output}, HasPrice: true, TextGeneration: true, Source: "embedded"})
	}
	sortEntries(entries)
	return entries
}
