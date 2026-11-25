package quota

import (
	"testing"
	"time"
)

func TestPeriodPreservesReportedDuration(t *testing.T) {
	for _, period := range []time.Duration{28 * 24 * time.Hour, 29 * 24 * time.Hour, 31 * 24 * time.Hour, 24 * time.Hour} {
		if got := Period(Window{ID: WindowThirtyDay, Period: period}); got != period {
			t.Fatalf("Period(%v) = %v", period, got)
		}
	}
	for id, want := range map[string]time.Duration{WindowFiveHour: FiveHours, WindowSevenDay: SevenDays, WindowThirtyDay: ThirtyDays} {
		for _, period := range []time.Duration{0, -time.Hour} {
			if got := Period(Window{ID: id, Period: period}); got != want {
				t.Fatalf("Period(%s, %v) = %v, want %v", id, period, got, want)
			}
		}
	}
}

func TestParseTime(t *testing.T) {
	cases := map[string]any{
		"seconds":      float64(1791064435),
		"milliseconds": float64(1791064435000),
		"string":       "1791064435",
		"iso":          "2026-10-03T21:53:55Z",
	}
	for name, value := range cases {
		if got := ParseTime(value).Unix(); got != 1791064435 {
			t.Errorf("%s: got %d", name, got)
		}
	}
	if !ParseTime("").IsZero() || !ParseTime(nil).IsZero() {
		t.Error("empty values must give zero time")
	}
}

func TestKindPrefersThePeriod(t *testing.T) {
	cases := []struct {
		w    Window
		want string
	}{
		{Window{ID: WindowFiveHour, Label: LabelFiveHour}, KindFiveHour},
		{Window{ID: "gemini-5h", Label: LabelFiveHour}, KindFiveHour},
		{Window{ID: WindowSevenDay}, KindSevenDay},
		{Window{ID: WindowThirtyDay}, KindThirtyDay},
		{Window{Label: LabelThirtyDay}, KindThirtyDay},
		{Window{ID: "calendar-month", Period: 31 * 24 * time.Hour}, KindThirtyDay},
		{Window{ID: "seven-day-opus", Label: "7 Day Opus"}, KindSevenDay},
		{Window{ID: WindowFable, Label: LabelFable}, KindSevenDay},
		{Window{ID: "limit-1", Label: "1d"}, ""},
		{Window{ID: "x", Label: "x", Period: 5 * time.Hour}, KindFiveHour},
		{Window{ID: "x", Label: "x", Period: 7*24*time.Hour + time.Minute}, KindSevenDay},
		{Window{ID: WindowFiveHour, Label: LabelFiveHour, Period: 24 * time.Hour}, ""},
		{Window{ID: "monthly", Label: "Month", Period: 30 * 24 * time.Hour}, KindThirtyDay},
	}
	for _, c := range cases {
		if got := Kind(c.w); got != c.want {
			t.Errorf("Kind(%+v) = %q, want %q", c.w, got, c.want)
		}
	}
	if id, label, ok := Standard(7 * 24 * time.Hour); !ok || id != WindowSevenDay || label != LabelSevenDay {
		t.Errorf("Standard(7d) = %s %s %v", id, label, ok)
	}
	if _, _, ok := Standard(24 * time.Hour); ok {
		t.Error("a day is not a standard window")
	}
}

func TestMonthlyWindowPeriods(t *testing.T) {
	for _, days := range []int{28, 29, 30, 31} {
		w := Window{Period: time.Duration(days) * 24 * time.Hour}
		if Kind(w) != KindThirtyDay || Class(w) != "long" || IsSevenDay(w) || Period(w) != w.Period {
			t.Errorf("%d days kind=%s class=%s", days, Kind(w), Class(w))
		}
	}
	for _, days := range []int{7, 26, 33} {
		if Kind(Window{Period: time.Duration(days) * 24 * time.Hour}) == KindThirtyDay {
			t.Errorf("%d days matched monthly", days)
		}
	}
}
