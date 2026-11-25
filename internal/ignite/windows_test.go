package ignite

import (
	"testing"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/quota"
	"github.com/NotRllyRn/cpa-pulse/internal/store"
)

func TestFutureWindowWaitsForItsReset(t *testing.T) {
	for _, period := range []time.Duration{quota.FiveHours, quota.SevenDays, quota.ThirtyDays} {
		for _, hour := range []int{5, 12, 23} {
			now := local(hour, 0)
			reset := local(12, 0).Add(period)
			due := schedule().DueAt(&store.TargetState{}, reset, now)
			if due.Before(reset.Add(3 * time.Second)) {
				t.Fatalf("period=%v now=%v reset=%v due=%v", period, now, reset, due)
			}
		}
	}
	// A reset outside active hours clips on the reset's date, not today's.
	reset := local(23, 0).AddDate(0, 0, 20)
	due := schedule().DueAt(&store.TargetState{}, reset, local(10, 0))
	if want := local(7, 0).AddDate(0, 0, 21); !due.Equal(want) {
		t.Fatalf("due=%v want=%v", due, want)
	}
}

func TestWindowDueNeedsEvidenceAndDeduplicatesServicedReset(t *testing.T) {
	now := local(10, 0)
	ts := &store.TargetState{}
	ws := &store.IgnitionWindowState{}
	for _, period := range []time.Duration{quota.SevenDays, quota.ThirtyDays, 24 * time.Hour} {
		if due := schedule().WindowDueAt(ts, ws, quota.Window{Period: period}, now); !due.IsZero() {
			t.Fatalf("invented reset: %v %v", period, due)
		}
	}
	if due := schedule().WindowDueAt(ts, ws, quota.Window{Period: quota.FiveHours}, now); !due.Equal(now) {
		t.Fatalf("lost 5h bootstrap: %v", due)
	}
	reset := now.Add(-time.Minute)
	ws.PulsedResetEpoch = reset.Unix()
	for _, period := range []time.Duration{quota.FiveHours, quota.SevenDays, quota.ThirtyDays} {
		if due := schedule().WindowDueAt(ts, ws, quota.Window{Period: period, Reset: reset}, now); !due.IsZero() {
			t.Fatalf("serviced reset repeated: %v", due)
		}
	}
}

func TestSharedProtectionAndObservation(t *testing.T) {
	now := local(10, 0)
	ts := &store.TargetState{LastSuccessEpoch: epoch(now), RetryAtEpoch: epoch(now.Add(10 * time.Minute)), CircuitOpenUntilEpoch: epoch(now.Add(time.Hour)), ConsecutiveFailures: 2}
	Observe(ts, now.Add(time.Hour), now, false)
	if ts.ConsecutiveFailures != 2 || ts.RetryAtEpoch == 0 || ts.CircuitOpenUntilEpoch == 0 {
		t.Fatalf("fixed short reset cleared shared failure: %+v", ts)
	}
	for _, period := range []time.Duration{quota.FiveHours, quota.SevenDays, quota.ThirtyDays} {
		due := schedule().WindowDueAt(ts, &store.IgnitionWindowState{}, quota.Window{Period: period, Reset: now.Add(-time.Minute)}, now)
		if !due.Equal(now.Add(time.Hour)) {
			t.Fatalf("%v bypassed shared protection: %v", period, due)
		}
	}
}

func TestLongRollingObservation(t *testing.T) {
	for _, period := range []time.Duration{quota.SevenDays, 28 * 24 * time.Hour, 29 * 24 * time.Hour, quota.ThirtyDays, 31 * 24 * time.Hour} {
		ws := &store.IgnitionWindowState{}
		w := quota.Window{Period: period}
		first := local(10, 0)
		if ObserveWindow(ws, first.Add(period), first, false, quota.Period(w)) {
			t.Fatal("one observation is insufficient")
		}
		second := first.Add(5 * time.Minute)
		w.Reset = second.Add(period)
		if !ObserveWindow(ws, w.Reset, second, false, quota.Period(w)) {
			t.Fatalf("rolling %v undetected", period)
		}
		if due := schedule().WindowDueAt(&store.TargetState{}, ws, w, second); !due.Equal(second) {
			t.Fatalf("rolling %v bootstrap due=%v, want %v", period, due, second)
		}
		if ObserveWindow(ws, w.Reset, second.Add(time.Minute), true, quota.Period(w)) {
			t.Fatal("passive window cannot be rolling")
		}
		if due := schedule().WindowDueAt(&store.TargetState{}, ws, w, second); !due.Equal(w.Reset.Add(3 * time.Second)) {
			t.Fatalf("fixed %v due=%v, want %v", period, due, w.Reset.Add(3*time.Second))
		}
	}
}

func TestCloseActiveRefreshPreservesRollingReset(t *testing.T) {
	for _, period := range []time.Duration{quota.FiveHours, quota.SevenDays, 28 * 24 * time.Hour, 29 * 24 * time.Hour, quota.ThirtyDays, 31 * 24 * time.Hour} {
		ws := &store.IgnitionWindowState{}
		first := local(10, 0)
		ObserveWindow(ws, first.Add(period), first, false, period)
		second := first.Add(time.Minute)
		if !ObserveWindow(ws, second.Add(period), second, false, period) {
			t.Fatalf("rolling %v undetected", period)
		}
		for _, delay := range []time.Duration{0, time.Second} {
			if !ObserveWindow(ws, second.Add(period), second.Add(delay), false, period) || !ws.RollingReset {
				t.Fatalf("%v refresh after %v lost rolling state", period, delay)
			}
		}
		if ObserveWindow(ws, second.Add(period), second.Add(time.Second), true, period) {
			t.Fatal("passive refresh must confirm a fixed window")
		}
	}
}
