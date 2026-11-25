package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/providers"
	"github.com/NotRllyRn/cpa-pulse/internal/quota"
	"github.com/NotRllyRn/cpa-pulse/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// windowEngine supplies real Codex parsing and group execution for selected windows.
func windowEngine(t *testing.T, windows []quota.Window, confirms map[string]bool) (*Engine, *fakeHost, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, testZone)}
	h := standardHost(c)
	h.auths = h.auths[:1]
	fired := false
	current := func() []quota.Window {
		out := append([]quota.Window{}, windows...)
		if fired {
			for i := range out {
				if confirms[out[i].ID] {
					out[i].Reset = c.t.Add(quota.Period(out[i]))
					out[i].Remaining = 99
				}
			}
		}
		return out
	}
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.URL != providers.CodexUsageURL {
			return pluginapi.HTTPResponse{StatusCode: 404}
		}
		rate := map[string]any{}
		for i, w := range current() {
			key := "primary_window"
			if i == 1 {
				key = "secondary_window"
			}
			raw := map[string]any{"used_percent": 100 - w.Remaining, "limit_window_seconds": quota.Period(w).Seconds()}
			if !w.Reset.IsZero() {
				raw["reset_at"] = w.Reset.Unix()
			}
			rate[key] = raw
		}
		body, _ := json.Marshal(map[string]any{"rate_limit": rate})
		return pluginapi.HTTPResponse{StatusCode: 200, Body: body}
	}
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		fired = true
		headers := http.Header{}
		for i, w := range current() {
			if !confirms[w.ID] {
				continue
			}
			prefix := "X-Codex-Primary-"
			if i == 1 {
				prefix = "X-Codex-Secondary-"
			}
			headers.Set(prefix+"Used-Percent", "1")
			headers.Set(prefix+"Window-Minutes", strconv.FormatInt(int64(quota.Period(w)/time.Minute), 10))
			headers.Set(prefix+"Reset-At", strconv.FormatInt(w.Reset.Unix(), 10))
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Headers: headers}, nil
	}
	e, _ := newTestEngine(t, h, c)
	e.poll(context.Background(), e.config(), c.t, "1", true)
	return e, h, c
}

func expiredPulseWindow(period time.Duration) quota.Window {
	id, label := quota.WindowFiveHour, quota.LabelFiveHour
	if period == quota.SevenDays {
		id, label = quota.WindowSevenDay, quota.LabelSevenDay
	}
	if period == quota.ThirtyDays {
		id, label = quota.WindowThirtyDay, quota.LabelThirtyDay
	}
	return quota.Window{ID: id, Label: label, Period: period, Remaining: 100, Reset: time.Date(2026, 11, 25, 9, 59, 0, 0, testZone)}
}

func TestGroupPulseStartsAllDueWindowsWithOneRequest(t *testing.T) {
	for _, periods := range [][]time.Duration{{quota.FiveHours}, {quota.SevenDays}, {quota.ThirtyDays}, {quota.FiveHours, quota.SevenDays}, {quota.FiveHours, quota.ThirtyDays}, {quota.SevenDays, quota.ThirtyDays}} {
		t.Run(time.Duration(periods[len(periods)-1]).String()+"/"+strconv.Itoa(len(periods)), func(t *testing.T) {
			windows := []quota.Window{}
			confirms := map[string]bool{}
			for _, period := range periods {
				w := expiredPulseWindow(period)
				windows = append(windows, w)
				confirms[w.ID] = true
			}
			e, h, c := windowEngine(t, windows, confirms)
			if !e.performDue(context.Background(), e.config()) || len(h.executed) != 1 {
				t.Fatalf("executions=%d", len(h.executed))
			}
			state := e.state.Target("codex:1:codex:main")
			for _, w := range windows {
				ws := state.Windows[w.ID]
				if ws == nil || ws.LastAttemptEpoch != c.t.Unix() || ws.LastSuccessEpoch != c.t.Unix() || ws.PulsedResetEpoch != w.Reset.Unix() {
					t.Fatalf("%s state=%+v", w.ID, ws)
				}
			}
			// Old quota can reappear after a request and after a restart.
			e.applyGroups(context.Background(), e.config(), e.creds["1"], []quota.RawGroup{{Key: "codex:main", SourceLabel: "GPT", Windows: windows}}, quota.SourceActive, c.t, c.t, false)
			c.add(2 * time.Minute)
			if e.performDue(context.Background(), e.config()) {
				t.Fatal("stale reset pulsed twice")
			}
			e.saveState()
			restarted, _ := newTestEngineIn(t, h, c, e.dataDir)
			if _, err := restarted.loadCreds(restarted.config()); err != nil {
				t.Fatal(err)
			}
			restarted.applyGroups(context.Background(), restarted.config(), restarted.creds["1"], []quota.RawGroup{{Key: "codex:main", SourceLabel: "GPT", Windows: windows}}, quota.SourceActive, c.t, c.t, false)
			if restarted.performDue(context.Background(), restarted.config()) || len(h.executed) != 1 {
				t.Fatal("restart repeated serviced reset")
			}
		})
	}
}

func TestPartialWindowConfirmationKeepsSuccessAndSharedFailure(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.SevenDays)
	e, h, c := windowEngine(t, []quota.Window{five, long}, map[string]bool{five.ID: true})
	if !e.performDue(context.Background(), e.config()) || len(h.executed) != 1 {
		t.Fatalf("executions=%d", len(h.executed))
	}
	ts := e.state.Target("codex:1:codex:main")
	if ts.Windows[five.ID].LastSuccessEpoch == 0 || ts.Windows[five.ID].PulsedResetEpoch != five.Reset.Unix() || ts.Windows[long.ID].LastSuccessEpoch != 0 || ts.Windows[long.ID].PulsedResetEpoch != 0 || ts.ConsecutiveFailures != 1 || ts.RetryAtEpoch == 0 {
		t.Fatalf("partial state=%+v windows=%+v", ts, ts.Windows)
	}
	retry := ts.RetryAtEpoch
	c.add(time.Minute)
	e.poll(context.Background(), e.config(), c.t, "1", true)
	if ts.RetryAtEpoch != retry || ts.ConsecutiveFailures != 1 || e.performDue(context.Background(), e.config()) || len(h.executed) != 1 {
		t.Fatal("active 5h cleared pending long protection")
	}
	e.refreshTargets(e.config())
	target := e.Status().Targets[0]
	if len(target.Windows) != 2 {
		t.Fatal(target)
	}
	for _, w := range target.Windows {
		if w.Kind == quota.KindSevenDay && (w.LastAttempt.IsZero() || !w.LastSuccess.IsZero()) {
			t.Fatalf("pending long status: %+v", w)
		}
	}
}

func TestActiveShortCannotConfirmPendingLong(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.ThirtyDays)
	five.Reset = five.Reset.Add(3 * time.Hour)
	e, h, _ := windowEngine(t, []quota.Window{five, long}, map[string]bool{five.ID: true})
	e.performDue(context.Background(), e.config())
	ts := e.state.Target("codex:1:codex:main")
	if len(h.executed) != 1 || ts.Windows[long.ID].LastSuccessEpoch != 0 || ts.LastSuccessEpoch != 0 || ts.ConsecutiveFailures != 1 {
		t.Fatalf("active short proved long: %+v", ts)
	}
}

func TestFutureLongResetDoesNotPulseOnDailyStart(t *testing.T) {
	for _, period := range []time.Duration{quota.SevenDays, quota.ThirtyDays} {
		w := expiredPulseWindow(period)
		w.Reset = w.Reset.Add(period)
		e, h, c := windowEngine(t, []quota.Window{w}, map[string]bool{w.ID: true})
		for day := 0; day < 3; day++ {
			if day > 0 {
				c.add(24 * time.Hour)
			}
			if e.performDue(context.Background(), e.config()) {
				t.Fatalf("%v fired on day %d", period, day)
			}
			e.refreshTargets(e.config())
			if due := e.Status().Targets[0].NextDue; due.Before(w.Reset) {
				t.Fatalf("next=%v reset=%v", due, w.Reset)
			}
		}
		if len(h.executed) != 0 {
			t.Fatal("future fixed reset pulsed")
		}
	}
}

func TestWindowSwitchesAndUnknownLongReset(t *testing.T) {
	for _, classes := range [][]string{{}, {"short"}, {"long"}, {"short", "long"}} {
		five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.SevenDays)
		e, _, _ := windowEngine(t, []quota.Window{five, long}, map[string]bool{five.ID: true, long.ID: true})
		cfg := e.config()
		cfg.Ignition.Windows = classes
		e.refreshTargets(cfg)
		statuses := e.Status().Targets[0].Windows
		if len(statuses) != 2 {
			t.Fatal("disabled class disappeared")
		}
		for _, w := range statuses {
			enabled := false
			for _, class := range classes {
				enabled = enabled || class == w.Class
			}
			if w.Enabled != enabled || w.NextDue.IsZero() == enabled {
				t.Fatalf("classes=%v window=%+v", classes, w)
			}
		}
		if got := e.performDue(context.Background(), cfg); got != (len(classes) > 0) {
			t.Fatalf("classes=%v performed=%v", classes, got)
		}
	}
	for _, period := range []time.Duration{quota.SevenDays, quota.ThirtyDays} {
		w := expiredPulseWindow(period)
		w.Reset = time.Time{}
		e, h, _ := windowEngine(t, []quota.Window{w}, nil)
		if e.performDue(context.Background(), e.config()) || len(h.executed) > 0 {
			t.Fatal("unknown long reset invented")
		}
	}
}

func TestSharedWindowGates(t *testing.T) {
	for _, gate := range []string{"retry", "circuit", "credential", "cooldown", "short-exhausted", "long-exhausted"} {
		t.Run(gate, func(t *testing.T) {
			five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.ThirtyDays)
			if gate == "short-exhausted" {
				five.Remaining = 0
				five.Reset = five.Reset.Add(time.Hour)
			}
			if gate == "long-exhausted" {
				long.Remaining = 0
				long.Reset = long.Reset.Add(quota.ThirtyDays)
			}
			e, h, c := windowEngine(t, []quota.Window{five, long}, nil)
			ts := e.state.Target("codex:1:codex:main")
			switch gate {
			case "retry":
				ts.RetryAtEpoch = float64(c.t.Add(time.Hour).Unix())
			case "circuit":
				ts.CircuitOpenUntilEpoch = float64(c.t.Add(time.Hour).Unix())
			case "credential":
				e.state.Credential("1").Reason = "auth"
			case "cooldown":
				e.creds["1"].CooldownUntil = c.t.Add(time.Hour)
			}
			if e.performDue(context.Background(), e.config()) || len(h.executed) > 0 {
				t.Fatalf("%s bypassed", gate)
			}
		})
	}
}

func TestShortObservationKeepsLongRequestBackoff(t *testing.T) {
	five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.SevenDays)
	e, h, c := windowEngine(t, []quota.Window{five, long}, nil)
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, errors.New("connection reset")
	}
	if !e.performDue(context.Background(), e.config()) {
		t.Fatal("missing attempt")
	}
	ts := e.state.Target("codex:1:codex:main")
	retry := ts.RetryAtEpoch
	five.Reset = c.t.Add(3 * time.Hour)
	c.add(time.Minute)
	e.applyGroups(context.Background(), e.config(), e.creds["1"], []quota.RawGroup{{Key: "codex:main", SourceLabel: "GPT", Windows: []quota.Window{five, long}}}, quota.SourceActive, c.t, c.t, false)
	if ts.RetryAtEpoch != retry || ts.ConsecutiveFailures != 1 || e.performDue(context.Background(), e.config()) {
		t.Fatal("fixed short observation bypassed long retry")
	}
}

func TestParentScheduleUsesEarliestEnabledWindowAndIDTieBreak(t *testing.T) {
	five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.ThirtyDays)
	e, _, c := windowEngine(t, []quota.Window{long, five}, nil)
	e.refreshTargets(e.config())
	view := e.Status().Targets[0]
	if !view.NextDue.Equal(c.t) || !view.Reset.Equal(five.Reset) || view.Windows[0].ID != five.ID {
		t.Fatalf("parent=%+v", view)
	}
	cfg := e.config()
	cfg.Ignition.Windows = []string{"long"}
	e.refreshTargets(cfg)
	view = e.Status().Targets[0]
	if !view.Reset.Equal(long.Reset) || !view.NextDue.Equal(c.t) {
		t.Fatalf("parent ignored long: %+v", view)
	}
	// Returned slices are owned by the caller.
	view.Windows[0].Enabled = true
	if e.Status().Targets[0].Windows[0].Enabled {
		t.Fatal("status child slice aliases engine state")
	}
}

func TestHistoryThirtyDayStrictFilterAndDefaultRange(t *testing.T) {
	windows := []quota.Window{expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.ThirtyDays)}
	e, _, c := windowEngine(t, windows, nil)
	if err := e.history.Append(store.Record{Kind: "s", Time: c.t.Add(-30 * 24 * time.Hour).Unix(), Group: "retired", Window: quota.WindowThirtyDay, Remaining: 75}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "31d"} {
		response, err := e.HistoryRange(name, quota.KindThirtyDay)
		if err != nil || !response.From.Equal(c.t.Add(-31*24*time.Hour)) || len(response.Series) != 2 {
			t.Fatalf("range=%q response=%+v err=%v", name, response, err)
		}
		for _, s := range response.Series {
			if s.Kind != quota.KindThirtyDay {
				t.Fatalf("wrong series: %+v", s)
			}
		}
	}
	for name, days := range map[string]int{"15d": 15, "31d": 31, "61d": 61, "90d": 90, "151d": 151} {
		if span := RangeSpan(name, c.t, time.UTC); span != time.Duration(days)*24*time.Hour {
			t.Fatalf("range %s=%v", name, span)
		}
	}
}

func TestConfirmRejectsObservationsReadBeforePulse(t *testing.T) {
	w := expiredPulseWindow(quota.ThirtyDays)
	e, _, c := windowEngine(t, []quota.Window{w}, nil)
	chosen := e.collectTargets(e.config())[0]
	chosen.due = []WindowView{{Window: w}}
	chosen.confirmed = map[string]bool{}
	group := e.groups[chosen.group.Key]
	group.Windows[0].Reset = c.t.Add(quota.ThirtyDays)
	group.Windows[0].ObservedAt = c.t
	group.Windows[0].ReadAt = c.t.Add(-time.Second)
	if e.confirmWindows(chosen, c.t, c.t, false) {
		t.Fatal("old request confirmed new pulse")
	}
	if e.state.Target(chosen.group.Key).Windows[w.ID].LastSuccessEpoch != 0 {
		t.Fatal("stale observation recorded success")
	}
}

func TestConfirmedLongSurvivesUnconfirmedShort(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.ThirtyDays)
	e, h, _ := windowEngine(t, []quota.Window{five, long}, map[string]bool{long.ID: true})
	e.performDue(context.Background(), e.config())
	ts := e.state.Target("codex:1:codex:main")
	if len(h.executed) != 1 || ts.Windows[long.ID].LastSuccessEpoch == 0 || ts.Windows[long.ID].PulsedResetEpoch != long.Reset.Unix() || ts.Windows[five.ID].LastSuccessEpoch != 0 || ts.ConsecutiveFailures != 1 {
		t.Fatalf("confirmed long lost to short failure: %+v windows=%+v", ts, ts.Windows)
	}
}

func TestManualPulseInsideActiveWindowKeepsUpcomingResetEligible(t *testing.T) {
	five := expiredPulseWindow(quota.FiveHours)
	five.Reset = five.Reset.Add(3 * time.Hour)
	e, h, c := windowEngine(t, []quota.Window{five}, nil)
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Headers: http.Header{
			"X-Codex-Primary-Used-Percent":   {"1"},
			"X-Codex-Primary-Window-Minutes": {"300"},
			"X-Codex-Primary-Reset-At":       {strconv.FormatInt(five.Reset.Unix(), 10)},
		}}, nil
	}
	chosen := e.collectTargets(e.config())[0]
	if err := e.igniteTarget(context.Background(), e.config(), chosen, true); err != nil {
		t.Fatal(err)
	}
	ws := e.state.Target(chosen.group.Key).Windows[five.ID]
	if ws.LastSuccessEpoch != c.t.Unix() || ws.PulsedResetEpoch != 0 {
		t.Fatalf("manual active pulse consumed future reset: %+v", ws)
	}
	e.refreshTargets(e.config())
	if due := e.Status().Targets[0].NextDue; !due.Equal(five.Reset.Add(3 * time.Second)) {
		t.Fatalf("upcoming reset lost: %v", due)
	}
}

func TestManualRetryConfirmsPendingLongDespiteSharedCircuit(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	for _, confirmed := range []bool{false, true} {
		t.Run(strconv.FormatBool(confirmed), func(t *testing.T) {
			five, long := expiredPulseWindow(quota.FiveHours), expiredPulseWindow(quota.SevenDays)
			five.Reset = five.Reset.Add(3 * time.Hour)
			e, _, c := windowEngine(t, []quota.Window{five, long}, map[string]bool{five.ID: true, long.ID: confirmed})
			chosen := e.collectTargets(e.config())[0]
			ts := e.state.Target(chosen.group.Key)
			ts.ConsecutiveFailures = 2
			ts.RetryAtEpoch = float64(c.t.Add(time.Hour).Unix())
			ts.CircuitOpenUntilEpoch = ts.RetryAtEpoch
			err := e.igniteTarget(context.Background(), e.config(), chosen, true)
			if !confirmed {
				if err == nil || ts.ConsecutiveFailures != 2 || ts.Windows[long.ID].LastSuccessEpoch != 0 || ts.Windows[long.ID].LastAttemptEpoch != c.t.Unix() || ts.Windows[five.ID].LastSuccessEpoch != 0 {
					t.Fatalf("active short confirmed pending manual long: err=%v state=%+v windows=%+v", err, ts, ts.Windows)
				}
			} else if err != nil || ts.ConsecutiveFailures != 0 || ts.CircuitOpenUntilEpoch != 0 || ts.Windows[long.ID].LastSuccessEpoch == 0 || ts.Windows[long.ID].PulsedResetEpoch != long.Reset.Unix() {
				t.Fatalf("manual long confirmation failed: err=%v state=%+v windows=%+v", err, ts, ts.Windows)
			}
		})
	}
}
