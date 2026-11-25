package engine

import (
	"sort"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/config"
	"github.com/NotRllyRn/cpa-pulse/internal/providers"
	"github.com/NotRllyRn/cpa-pulse/internal/quota"
)

// targetWindows describes all recognized windows, including disabled classes.
func (e *Engine) targetWindows(cfg config.Config, t target, now time.Time) []TargetWindowStatus {
	ts := e.state.Target(t.group.Key)
	schedule := e.schedule(cfg)
	out := []TargetWindowStatus{}
	_, exhausted := exhaustedWindow(t.group, now)
	blockedReset := exhaustedRetryReset(t.group, now)
	for _, w := range t.group.Windows {
		class := quota.Class(w.Window)
		if class == "" {
			continue
		}
		ws := ts.IgnitionWindow(w.ID, quota.IsFiveHour(w.Window))
		view := TargetWindowStatus{
			ID: w.ID, Kind: quota.Kind(w.Window), Class: class,
			Enabled: cfg.Ignition.Enabled && cfg.Ignition.WindowEnabled(class) && groupIgnitable(cfg, t.group.Provider, t.group.SourceLabel) && providers.Pulseable(t.group.Provider, t.group.SourceLabel, w.Window),
			Reset:   w.Reset, Rolling: ws.RollingReset,
			LastSuccess: epochTime(float64(ws.LastSuccessEpoch)), LastAttempt: epochTime(float64(ws.LastAttemptEpoch)),
		}
		if view.Enabled && (!exhausted || !blockedReset.IsZero()) && !e.credentialBlocked(t.cred.AuthIndex) {
			view.NextDue = schedule.WindowDueAt(ts, ws, w.Window, now)
			if !view.NextDue.IsZero() {
				gate := blockedReset
				if t.cred.CooldownUntil.After(gate) {
					gate = t.cred.CooldownUntil
				}
				if !gate.IsZero() {
					state := *ts
					state.RollingReset = false
					if ready := schedule.DueAt(&state, gate, now); ready.After(view.NextDue) {
						view.NextDue = ready
					}
				}
			}
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (e *Engine) dueWindows(cfg config.Config, t target, now time.Time) []WindowView {
	out := []WindowView{}
	for _, status := range e.targetWindows(cfg, t, now) {
		if status.NextDue.IsZero() || status.NextDue.After(now.Add(250*time.Millisecond)) {
			continue
		}
		for _, w := range t.group.Windows {
			if w.ID == status.ID {
				out = append(out, w)
			}
		}
	}
	return out
}

// confirmWindows checks every due window against its own fresh observation.
// Confirmed siblings keep their progress when another window remains pending.
func (e *Engine) confirmWindows(t target, start, now time.Time, manual bool) bool {
	group, ok := e.group(t.group.Key)
	if !ok || len(t.due) == 0 {
		return false
	}
	ts := e.state.Target(t.group.Key)
	for _, due := range t.due {
		if t.confirmed[due.ID] {
			continue
		}
		for _, w := range group.Windows {
			if w.ID != due.ID || w.ReadAt.Before(start) || w.ObservedAt.Before(start) || !w.Reset.After(now) {
				continue
			}
			ws := ts.IgnitionWindow(w.ID, quota.IsFiveHour(w.Window))
			if ws.RollingReset {
				continue
			}
			// An unchanged active long reset is not evidence that a pending
			// long cycle started, even if the short cycle is already running.
			if !manual && !t.rollingDue[due.ID] && !quota.IsFiveHour(w.Window) && !due.Reset.IsZero() && w.Reset.Equal(due.Reset) {
				continue
			}
			ws.LastSuccessEpoch, ws.LastAttemptEpoch = now.Unix(), start.Unix()
			if resetEpoch := due.Reset.Unix(); resetEpoch > 0 && !due.Reset.After(start) {
				ws.PulsedResetEpoch = resetEpoch
			}
			t.confirmed[w.ID] = true
		}
	}
	return len(t.confirmed) == len(t.due)
}
