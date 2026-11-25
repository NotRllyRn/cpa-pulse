package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEmptyUsesDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalSeconds != 300 || !cfg.Ignition.Enabled || cfg.Ignition.StartHour != 7 {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
	if !cfg.Provider("codex").Ignite || cfg.Provider("antigravity").Ignite {
		t.Fatal("codex ignites by default, antigravity does not")
	}
	if cfg.RememberChartFilters || cfg.RememberChartExpanded {
		t.Fatal("the chart state is not remembered by default")
	}
}

func TestParseRememberChartFilters(t *testing.T) {
	cfg, err := Parse([]byte("remember_chart_filters: true\nremember_chart_expanded: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RememberChartFilters || !cfg.RememberChartExpanded {
		t.Fatalf("remember switches not read: %+v", cfg)
	}
}

func TestPartialProviderBlockKeepsDefaults(t *testing.T) {
	cfg, err := Parse([]byte("enabled: true\npriority: 1\nproviders:\n  antigravity:\n    ignite: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	ag := cfg.Provider("antigravity")
	if !ag.Ignite || !ag.Monitor || len(ag.Groups) != 1 || ag.Groups[0] != "Gemini" {
		t.Fatalf("antigravity %+v", ag)
	}
	if !cfg.Provider("claude").Ignite {
		t.Fatal("other providers keep their defaults")
	}
}

func TestParseClampsValues(t *testing.T) {
	cfg, err := Parse([]byte("poll_interval_seconds: 5\nignition:\n  start_hour: 30\n  max_transient_failures: 50\ncodex_reset_updates:\n  poll_seconds: 10\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollIntervalSeconds != 60 || cfg.Ignition.StartHour != 23 || cfg.Ignition.MaxTransientFailures != 10 || cfg.CodexResetUpdates.PollSeconds != 300 {
		t.Fatalf("not clamped: %+v", cfg)
	}
	if cfg.Ignition.EndHour != 22 {
		t.Fatal("keys missing from a partial block keep their defaults")
	}
}

func TestParseReplacesOldBarkIcon(t *testing.T) {
	cfg, err := Parse([]byte("bark_icon: " + oldBarkIcon + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BarkIcon != DefaultBarkIcon {
		t.Fatalf("bark_icon %q", cfg.BarkIcon)
	}
}

func TestParseRejectsInvalidYAML(t *testing.T) {
	if _, err := Parse([]byte("bark_url: [")); err == nil {
		t.Fatal("invalid YAML must fail")
	}
}

func TestHostConfigPath(t *testing.T) {
	if got := HostConfigPath([]string{"./CLIProxyAPI", "-config", "/etc/cpa.yaml"}, "/w"); got != "/etc/cpa.yaml" {
		t.Fatalf("got %q", got)
	}
	if got := HostConfigPath([]string{"./CLIProxyAPI", "--config=/etc/x.yaml"}, "/w"); got != "/etc/x.yaml" {
		t.Fatalf("got %q", got)
	}
	if got := HostConfigPath([]string{"./CLIProxyAPI"}, "/w"); got != filepath.Join("/w", "config.yaml") {
		t.Fatalf("got %q", got)
	}
}

func TestPluginEnabled(t *testing.T) {
	enabled, err := PluginEnabled([]byte("plugins:\n  enabled: true\n  configs:\n    cpa-pulse:\n      enabled: true\n"))
	if err != nil || !enabled {
		t.Fatalf("got %v %v", enabled, err)
	}
	enabled, _ = PluginEnabled([]byte("plugins:\n  enabled: true\n  configs:\n    cpa-pulse:\n      enabled: false\n"))
	if enabled {
		t.Fatal("disabled plugin reported as enabled")
	}
}

func TestPluginEnabledIgnoresSiblingConfig(t *testing.T) {
	siblingID := "lamp" + "lighter"
	for _, pulseConfig := range []string{"", "    cpa-pulse:\n      enabled: false\n"} {
		raw := "plugins:\n  enabled: true\n  configs:\n    " + siblingID + ":\n      enabled: true\n      data_dir: /sibling/data\n" + pulseConfig
		enabled, err := PluginEnabled([]byte(raw))
		if err != nil || enabled {
			t.Fatalf("sibling config enabled cpa-pulse: %v %v", enabled, err)
		}
	}
}

func TestPluginsDir(t *testing.T) {
	if got := PluginsDir(nil); got != "plugins" {
		t.Fatalf("default: got %q", got)
	}
	if got := PluginsDir([]byte("plugins:\n  dir: ./ext/plugins/\n")); got != filepath.Join("ext", "plugins") {
		t.Fatalf("relative: got %q", got)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	if got := PluginsDir([]byte("plugins:\n  dir: ~/cpa/plugins\n")); got != filepath.Join(home, "cpa", "plugins") {
		t.Fatalf("home: got %q", got)
	}
}

func TestDefaultDataDir(t *testing.T) {
	work := t.TempDir()
	if got, want := DefaultDataDir(filepath.Join(work, "missing.yaml"), work), filepath.Join(work, "plugins", "data", "cpa-pulse"); got != want {
		t.Fatalf("missing config: got %q, want %q", got, want)
	}
	cfgPath := filepath.Join(work, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("plugins:\n  dir: ext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := DefaultDataDir(cfgPath, work), filepath.Join(work, "ext", "data", "cpa-pulse"); got != want {
		t.Fatalf("relative: got %q, want %q", got, want)
	}
	abs := filepath.Join(t.TempDir(), "plugins")
	if err := os.WriteFile(cfgPath, []byte("plugins:\n  dir: "+filepath.ToSlash(abs)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := DefaultDataDir(cfgPath, work), filepath.Join(abs, "data", "cpa-pulse"); got != want {
		t.Fatalf("absolute: got %q, want %q", got, want)
	}
}

func TestNotificationModes(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryNotify != (WindowModes{FiveHour: RecoveryAfterExhausted, SevenDay: RecoveryAll}) ||
		cfg.ResetReminder != (WindowModes{FiveHour: ReminderOff, SevenDay: ReminderHasRemaining}) {
		t.Fatalf("default modes: %+v %+v", cfg.RecoveryNotify, cfg.ResetReminder)
	}
	cfg, err = Parse([]byte("recovery_notify:\n  five_hour: after_exhausted\n  seven_day: bogus\nreset_reminder:\n  seven_day: has_remaining\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryNotify != (WindowModes{FiveHour: RecoveryAfterExhausted, SevenDay: RecoveryOff}) {
		t.Fatalf("recovery_notify %+v", cfg.RecoveryNotify)
	}
	// A key missing from a partial block keeps its default.
	if cfg.ResetReminder != (WindowModes{FiveHour: ReminderOff, SevenDay: ReminderHasRemaining}) {
		t.Fatalf("reset_reminder %+v", cfg.ResetReminder)
	}
	cfg, err = Parse([]byte("reset_reminder:\n  five_hour: all\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ResetReminder != (WindowModes{FiveHour: ReminderAll, SevenDay: ReminderHasRemaining}) {
		t.Fatalf("partial reset_reminder %+v", cfg.ResetReminder)
	}
}

func TestLegacyNotificationSwitches(t *testing.T) {
	cfg, err := Parse([]byte("notify_recovery: true\nnotify_reset_reminders: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryNotify != (WindowModes{FiveHour: RecoveryAll, SevenDay: RecoveryAll}) ||
		cfg.ResetReminder != (WindowModes{FiveHour: ReminderAll, SevenDay: ReminderAll}) {
		t.Fatalf("legacy switches map to all: %+v %+v", cfg.RecoveryNotify, cfg.ResetReminder)
	}
	cfg, err = Parse([]byte("notify_recovery: true\nrecovery_notify:\n  seven_day: after_exhausted\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryNotify != (WindowModes{FiveHour: RecoveryAfterExhausted, SevenDay: RecoveryAfterExhausted}) {
		t.Fatalf("the new key wins over the legacy switch: %+v", cfg.RecoveryNotify)
	}
	cfg, err = Parse([]byte("notify_recovery: false\nnotify_reset_reminders: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryNotify != (WindowModes{FiveHour: RecoveryOff, SevenDay: RecoveryOff}) ||
		cfg.ResetReminder != (WindowModes{FiveHour: ReminderOff, SevenDay: ReminderOff}) {
		t.Fatalf("legacy switches set to false keep both windows off: %+v %+v", cfg.RecoveryNotify, cfg.ResetReminder)
	}
}

func TestProviderDefaultsAndAliases(t *testing.T) {
	cfg := Default()
	if p := cfg.Provider("kimi"); !p.Monitor || p.Ignite {
		t.Fatalf("Kimi is monitored and cannot be ignited: %+v", p)
	}
	if p := cfg.Provider("xai"); !p.Monitor || p.Ignite {
		t.Fatalf("Grok is monitored and cannot be ignited: %+v", p)
	}
	cfg, err := Parse([]byte("providers:\n  kimi-ai:\n    monitor: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Provider("kimi"); p.Monitor {
		t.Fatalf("an alias key must set the provider: %+v", p)
	}
	if _, ok := cfg.Providers["kimi-ai"]; ok {
		t.Fatal("the alias key must be stored under the provider ID")
	}
}

func TestProviderIDKeyWinsOverAnAlias(t *testing.T) {
	raw := []byte("providers:\n  kimi-ai:\n    monitor: false\n  kimi:\n    monitor: true\n")
	for i := 0; i < 50; i++ {
		cfg, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Provider("kimi").Monitor {
			t.Fatal("the kimi key must win over kimi-ai")
		}
	}
}

func TestParseAccountWeightsDropsInvalidWeights(t *testing.T) {
	cfg, err := Parse([]byte("account_weights:\n  a.json: 5\n  b.json: 0.5\n  c.json: 0\n  d.json: -2\n  e.json: .inf\n  f.json: .nan\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AccountWeights) != 2 || cfg.AccountWeights["a.json"] != 5 || cfg.AccountWeights["b.json"] != 0.5 {
		t.Fatalf("account weights %v", cfg.AccountWeights)
	}
}

func TestPricingConfigDefaultsBoundsAndAccountModels(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		hours int
	}{
		{"", 24}, {"pricing: {}", 24}, {"pricing: {refresh_hours: 0}", 0},
		{"pricing: {refresh_hours: 168}", 168}, {"pricing: {refresh_hours: 999}", 168}, {"pricing: {refresh_hours: -1}", 0},
	} {
		cfg, err := Parse([]byte(tc.raw))
		if err != nil || cfg.Pricing.RefreshHours != tc.hours {
			t.Fatalf("%q: hours=%d err=%v", tc.raw, cfg.Pricing.RefreshHours, err)
		}
	}
	cfg, err := Parse([]byte("account_models:\n  sample.json:\n    'claude:3:claude:main': claude-haiku-5-5\n"))
	if err != nil || cfg.AccountModels["sample.json"]["claude:3:claude:main"] != "claude-haiku-5-5" {
		t.Fatalf("account models: %+v %v", cfg.AccountModels, err)
	}
}
