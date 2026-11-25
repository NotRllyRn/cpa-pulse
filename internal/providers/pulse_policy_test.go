package providers

import (
	"testing"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/quota"
)

func TestPulseableWindowPolicy(t *testing.T) {
	for _, tc := range []struct {
		provider, group, id string
		period              time.Duration
		want                bool
	}{
		{"claude", "Claude", quota.WindowFiveHour, quota.FiveHours, true},
		{"claude", "Claude", quota.WindowSevenDay, quota.SevenDays, true},
		{"claude", "Claude", quota.WindowThirtyDay, quota.ThirtyDays, false},
		{"claude", "Fable", quota.WindowFable, quota.SevenDays, false},
		{"claude", "7 Day Opus", "seven-day-opus", quota.SevenDays, false},
		{"codex", "GPT", quota.WindowSevenDay, quota.SevenDays, true},
		{"codex", "GPT", quota.WindowThirtyDay, quota.ThirtyDays, true},
		{"codex", "GPT", "window-1d", 24 * time.Hour, false},
		{"codex", "scoped", quota.WindowSevenDay, quota.SevenDays, false},
		{"antigravity", "Gemini", "bucket-30d", quota.ThirtyDays, true},
		{"antigravity", "Claude / GPT", "bucket-week", quota.SevenDays, true},
		{"antigravity", "Other", "bucket-week", quota.SevenDays, false},
		{"kimi", "Kimi", quota.WindowFiveHour, quota.FiveHours, false},
		{"xai", "Grok", quota.WindowThirtyDay, quota.ThirtyDays, false},
	} {
		if got := Pulseable(tc.provider, tc.group, quota.Window{ID: tc.id, Period: tc.period}); got != tc.want {
			t.Errorf("%s/%s/%s=%v", tc.provider, tc.group, tc.id, got)
		}
	}
}

func TestAntigravityThirtyDayWindows(t *testing.T) {
	groups, err := ParseAntigravityQuota(decode(t, `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"existing-month-pool","window":"30d","remainingFraction":0.8,"resetTime":"2026-12-25T00:00:00Z"}]}]}`))
	if err != nil || len(groups) != 1 || len(groups[0].Windows) != 1 {
		t.Fatalf("groups=%+v err=%v", groups, err)
	}
	w := groups[0].Windows[0]
	if w.ID != "existing-month-pool" || w.Period != quota.ThirtyDays || quota.Kind(w) != quota.KindThirtyDay || quota.ShortLabel(w.Label) != "30d" {
		t.Fatalf("window=%+v", w)
	}
}
