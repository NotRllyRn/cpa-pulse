package notify

import (
	"fmt"
	"time"
)

// LangEN is the notification language.
const LangEN = "en"

// NormalizeLang returns English for every input.
func NormalizeLang(_ string) string {
	return LangEN
}

// weekdays holds the short English weekday names, Sunday first.
var weekdays = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// FormatTime formats t in loc as month/day, weekday and time, such as
// "11/23 Mon 03:00". The language argument is kept for compatibility.
func FormatTime(t time.Time, loc *time.Location, _ string) string {
	local := t.In(loc)
	return local.Format("01/02") + " " + weekdays[local.Weekday()] + " " + local.Format("15:04")
}

// texts holds the English notification copy.
var texts = map[string]string{
	"due":                  "due",
	"window_line":          "%s: %s",
	"window_line_reset":    "%s: %s | %s | %s",
	"recovered":            "✅ %s · %s recovered",
	"previous_cycle":       "Left last cycle: %s",
	"reset_reminder":       "⏰ %s · %s resets soon",
	"cooldown_title":       "⚠️ %s quota is back, CPA still cooling down",
	"cooldown_body":        "CPA cools this account down until %s and rejects requests and ignition until then. Clear the cooldown of this account in the CPA Management Center.",
	"circuit_title":        "⚠️ %s ignition paused",
	"circuit_body":         "Repeated or high-risk errors tripped the protection; automatic retries stop until %s.\n%s",
	"plan_title":           "🎫 %s · plan %s → %s",
	"plan_body":            "Quota alerts of this account start over with the quota of the new plan.",
	"credential_title":     "🔑 %s credential invalid",
	"credential_fix":       "Monitoring and ignition resume after you log in again or update the credential.",
	"suspended_title":      "⛔ %s account suspended",
	"suspended_reason":     "the account was suspended; an appeal is required",
	"rate_limited_title":   "⏳ %s rate limited",
	"rate_limited_until":   "Automatic ignition paused until %s.",
	"credential_ok_title":  "✅ %s credential works again",
	"credential_ok_query":  "Quota queries work again.",
	"credential_ok_ignite": "Ignition succeeded; automatic ignition resumed.",
	"line_source":          "Source: %s",
	"line_appeal":          "Appeal: %s",
	"source_ignite":        "ignition",
	"source_query":         "quota query",
	"test_title":           "🕯️ cpa-pulse test notification",
	"test_body":            "Bark notifications are set up correctly.",
	"reset_global":         "global reset",
	"reset_banked":         "reset card",
	"reset_both":           "global reset + reset card",
	"reset_other":          "reset",
	"plans_all":            "all plans",
	"reset_scheduled":      "📅 Codex %s scheduled",
	"reset_rescheduled":    "📅 Codex %s schedule updated",
	"reset_done":           "✅ Codex %s done",
	"reset_credited":       "✅ Codex %s credited",
	"line_expected":        "Expected: %s",
	"schedule_range":       "%s–%s",
	"schedule_before":      "by %s",
	"line_time":            "Time: %s",
	"line_confidence":      "Confidence: %s",
	"line_scope":           "Plans: %s",
	"reset_signal":         "Did Codex Reset published a new reset signal",
	"reset_history_link":   "https://didcodexreset.com/history/%d.html",
}

// T returns the English text for key, formatted with args.
// The language argument is kept for compatibility.
func T(_ string, key string, args ...any) string {
	format := texts[key]
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}
