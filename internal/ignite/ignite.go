// Package ignite schedules usage-triggered quota windows and keeps shared
// request failure protection. Requests run after reset plus grace, inside
// the configured daily hours.
package ignite

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/config"
	"github.com/NotRllyRn/cpa-pulse/internal/failure"
	"github.com/NotRllyRn/cpa-pulse/internal/host"
	"github.com/NotRllyRn/cpa-pulse/internal/quota"
	"github.com/NotRllyRn/cpa-pulse/internal/store"
)

// Some providers report an unstarted 5-hour window as a rolling placeholder:
// the reset time stays about 5 hours ahead and moves forward with every
// observation. A started window has a fixed reset time. Two observations tell
// them apart.
const (
	rollingOffsetTolerance   = 120 * time.Second
	rollingMinObservation    = 2 * time.Second
	rollingDriftTolerance    = 3 * time.Second
	rollingMaxDriftTolerance = 10 * time.Second
)

// Schedule computes due times from the ignition settings.
type Schedule struct {
	Cfg config.Ignition
	Loc *time.Location
}

func (s Schedule) bounds(now time.Time) (localNow, start, end time.Time) {
	localNow = now.In(s.Loc)
	start = time.Date(localNow.Year(), localNow.Month(), localNow.Day(), s.Cfg.StartHour, 0, 0, 0, s.Loc)
	end = time.Date(localNow.Year(), localNow.Month(), localNow.Day(), s.Cfg.EndHour, 0, 0, 0, s.Loc).
		Add(time.Duration(s.Cfg.EndGraceMinutes) * time.Minute)
	if !end.After(start) {
		end = end.AddDate(0, 0, 1)
	}
	return localNow, start, end
}

// NextDailyStart returns the next start_hour after now.
func (s Schedule) NextDailyStart(now time.Time) time.Time {
	localNow := now.In(s.Loc)
	start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), s.Cfg.StartHour, 0, 0, 0, s.Loc)
	if !localNow.Before(start) {
		start = start.AddDate(0, 0, 1)
	}
	return start.UTC()
}

func epochTime(epoch float64) time.Time {
	if epoch <= 0 {
		return time.Time{}
	}
	sec, frac := math.Modf(epoch)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

func epoch(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

// HeldUntil returns the end of the post-success hold or of the retry delay,
// or the zero time when the target is not held.
func (s Schedule) HeldUntil(ts *store.TargetState, now time.Time) time.Time {
	var until time.Time
	for _, candidate := range []time.Time{
		epochTime(ts.LastSuccessEpoch).Add(time.Duration(s.Cfg.PostSuccessHoldSeconds) * time.Second),
		epochTime(ts.RetryAtEpoch), epochTime(ts.CircuitOpenUntilEpoch),
	} {
		if candidate.After(now) && candidate.After(until) {
			until = candidate
		}
	}
	return until
}

// activeAt moves a candidate forward into the configured daily hours.
func (s Schedule) activeAt(candidate time.Time) time.Time {
	local, start, end := s.bounds(candidate)
	if local.Before(start) {
		// Overnight hours may belong to the preceding day's interval.
		_, previousStart, previousEnd := s.bounds(candidate.In(s.Loc).AddDate(0, 0, -1))
		if !local.Before(previousStart) && !local.After(previousEnd) {
			return candidate.UTC()
		}
		return start.UTC()
	}
	if local.After(end) {
		return s.NextDailyStart(candidate)
	}
	return candidate.UTC()
}

// DueAt returns the reset plus grace, constrained by shared protection and
// daily hours. A future reset is never replaced by an earlier daily start.
func (s Schedule) DueAt(ts *store.TargetState, reset, now time.Time) time.Time {
	candidate := now
	if !ts.RollingReset && !reset.IsZero() {
		if ready := reset.Add(time.Duration(s.Cfg.GraceSeconds) * time.Second); ready.After(candidate) {
			candidate = ready
		}
	}
	if hold := s.HeldUntil(ts, now); hold.After(candidate) {
		candidate = hold
	}
	return s.activeAt(candidate)
}

// WindowDueAt returns the next eligible time of one observed window.
// Long windows need a reset observation; serviced stale resets wait for new data.
func (s Schedule) WindowDueAt(ts *store.TargetState, ws *store.IgnitionWindowState, w quota.Window, now time.Time) time.Time {
	if quota.Class(w) == "" || (quota.Class(w) == "long" && w.Reset.IsZero()) {
		return time.Time{}
	}
	if !w.Reset.IsZero() && ws.PulsedResetEpoch != 0 && w.Reset.Unix() == ws.PulsedResetEpoch {
		return time.Time{}
	}
	state := *ts
	state.RollingReset = ws.RollingReset
	return s.DueAt(&state, w.Reset, now)
}

// Observe records one 5-hour reset observation without changing shared failures.
func Observe(ts *store.TargetState, reset, now time.Time, passive bool) bool {
	ws := &store.IgnitionWindowState{RollingReset: ts.RollingReset, ResetObservation: ts.ResetObservation}
	rolling := ObserveWindow(ws, reset, now, passive, quota.FiveHours)
	ts.RollingReset, ts.ResetObservation = ws.RollingReset, ws.ResetObservation
	return rolling
}

// ObserveWindow detects rolling placeholders using the actual window period.
func ObserveWindow(ts *store.IgnitionWindowState, reset, now time.Time, passive bool, period time.Duration) bool {
	rolling := false
	var resetEpoch *float64
	if !reset.IsZero() {
		value := epoch(reset)
		resetEpoch = &value
		prev := ts.ResetObservation
		offset := reset.Sub(now)
		// Passive observations come from a real request, which always starts
		// the window, so they are never rolling.
		if !passive && prev != nil && prev.ResetEpoch != nil {
			elapsed := time.Duration((epoch(now) - prev.SeenEpoch) * float64(time.Second))
			shift := time.Duration((value - *prev.ResetEpoch) * float64(time.Second))
			if elapsed < rollingMinObservation {
				rolling = ts.RollingReset
			} else if absDuration(offset-period) <= rollingOffsetTolerance {
				tolerance := time.Duration(float64(elapsed) * 0.02)
				if tolerance < rollingDriftTolerance {
					tolerance = rollingDriftTolerance
				}
				if tolerance > rollingMaxDriftTolerance {
					tolerance = rollingMaxDriftTolerance
				}
				rolling = absDuration(shift-elapsed) <= tolerance
			}
		}
	}
	ts.RollingReset = rolling
	ts.ResetObservation = &store.Observation{SeenEpoch: epoch(now), ResetEpoch: resetEpoch}
	return rolling
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// ClearFailures resets the failure counter and closes the circuit.
func ClearFailures(ts *store.TargetState) {
	ts.ConsecutiveFailures = 0
	ts.RetryAtEpoch = 0
	ts.CircuitOpenUntilEpoch = 0
	ts.CircuitReason = ""
	ts.CircuitNotifiedUntilEpoch = 0
}

// RecordSuccess stores a confirmed ignition.
func RecordSuccess(ts *store.TargetState, model string, now time.Time) {
	ts.LastSuccessEpoch = epoch(now)
	ts.LastModel = model
	ts.LastError = ""
	ClearFailures(ts)
}

// FailureOutcome describes what RecordFailure decided.
type FailureOutcome struct {
	CircuitOpened bool
	// NotifyCircuit is true the first time the circuit opens for a given
	// pause, so that only one notification is sent.
	NotifyCircuit bool
	Until         time.Time
	RetryIn       time.Duration
	// Cooldown is true when the retry waits for a CPA cooldown and the
	// failure was not counted.
	Cooldown bool
}

// maxCooldownWait bounds the CPA cooldown that defers a retry. A longer
// cooldown means the quota is still exhausted, which the normal retry and
// failure protection handle.
const maxCooldownWait = 10 * time.Minute

var resetSecondsPattern = regexp.MustCompile(`"reset_seconds"\s*:\s*(\d+)`)

// CooldownWait returns how long CPA keeps the credential in its local
// cooldown, from the reset_seconds of a model_cooldown error. CPA cools a
// credential down after a 429 until the provider's reset plus a margin, so
// an ignition at reset plus grace can arrive a few seconds too early.
func CooldownWait(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	text := err.Error()
	if !strings.Contains(text, "model_cooldown") && !strings.Contains(text, "are cooling down") {
		return 0, false
	}
	match := resetSecondsPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	seconds, convErr := strconv.Atoi(match[1])
	if convErr != nil || seconds <= 0 {
		return 0, false
	}
	wait := time.Duration(seconds) * time.Second
	return wait, wait <= maxCooldownWait
}

// RecordFailure updates the failure state. Hard errors and the last allowed
// transient error pause the target until the next daily start.
//
// A short CPA cooldown is waited out instead: the retry runs when the
// cooldown ends plus the grace period, and the failure is not counted. Only
// the first of consecutive cooldowns gets this, so a cooldown that keeps
// renewing still reaches the failure protection.
func (s Schedule) RecordFailure(ts *store.TargetState, now time.Time, err error) FailureOutcome {
	if wait, ok := CooldownWait(err); ok && !s.recentCooldown(ts, now) {
		delay := wait + time.Duration(s.Cfg.GraceSeconds)*time.Second
		ts.LastError = truncate(errorText(err), 500)
		ts.LastFailureEpoch = epoch(now)
		ts.RetryAtEpoch = epoch(now.Add(delay))
		return FailureOutcome{RetryIn: delay, Cooldown: true}
	}
	ts.ConsecutiveFailures++
	ts.LastError = truncate(errorText(err), 500)
	ts.LastFailureEpoch = epoch(now)
	if Classify(err) == Hard || ts.ConsecutiveFailures >= s.Cfg.MaxTransientFailures {
		until := s.NextDailyStart(now)
		untilEpoch := float64(until.Unix())
		ts.CircuitOpenUntilEpoch = untilEpoch
		ts.RetryAtEpoch = untilEpoch
		ts.CircuitReason = truncate(errorText(err), 500)
		notify := ts.CircuitNotifiedUntilEpoch != untilEpoch
		ts.CircuitNotifiedUntilEpoch = untilEpoch
		return FailureOutcome{CircuitOpened: true, NotifyCircuit: notify, Until: until}
	}
	delay := time.Duration(s.Cfg.FailureRetrySeconds) * time.Second
	for i := 1; i < ts.ConsecutiveFailures; i++ {
		delay *= time.Duration(s.Cfg.FailureBackoffMultiplier)
	}
	ts.RetryAtEpoch = epoch(now.Add(delay))
	return FailureOutcome{RetryIn: delay}
}

// RecordCredentialFailure records a failure caused by the credential: the
// provider rejected it or disabled the account. Retrying cannot help, so no
// retry time is set; the engine skips the target until the credential
// changes or a manual ignition succeeds. summary is the error in one
// sentence, shown as the pause reason.
func RecordCredentialFailure(ts *store.TargetState, now time.Time, err error, summary string) {
	ts.ConsecutiveFailures++
	ts.LastError = truncate(errorText(err), 500)
	ts.LastFailureEpoch = epoch(now)
	ts.RetryAtEpoch = 0
	ts.CircuitOpenUntilEpoch = 0
	ts.CircuitReason = truncate(summary, 500)
}

// recentCooldown reports whether the previous failure, shortly before now,
// was already a CPA cooldown.
func (s Schedule) recentCooldown(ts *store.TargetState, now time.Time) bool {
	if ts.LastFailureEpoch == 0 || now.Sub(epochTime(ts.LastFailureEpoch)) > 2*maxCooldownWait {
		return false
	}
	_, ok := CooldownWait(errors.New(ts.LastError))
	return ok
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncate(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

// Kind classifies an ignition error.
type Kind int

const (
	// Transient errors are retried with backoff.
	Transient Kind = iota
	// Hard errors pause the target until the next daily start.
	Hard
	// Unsupported means CPA rejected the model for the pinned credential
	// before contacting the provider; the caller tries the next model.
	Unsupported
)

// ErrNotConfirmed means one or more requested windows could not be confirmed.
var ErrNotConfirmed = errors.New("ignition request returned without confirming all due quota windows")

// ErrNoModel means CPA rejected every candidate model for the credential.
var ErrNoModel = errors.New("no candidate ignition model is available for this credential")

var hardStatuses = map[int]bool{400: true, 401: true, 403: true, 404: true, 409: true, 422: true, 429: true}

// Classify sorts an ignition error into Transient, Hard or Unsupported.
func Classify(err error) Kind {
	if err == nil {
		return Transient
	}
	if errors.Is(err, ErrNotConfirmed) || errors.Is(err, ErrNoModel) {
		return Hard
	}
	text := strings.ToLower(err.Error())
	// CPA's scheduler answers "auth_not_found" when the pinned credential
	// does not serve the model, and the handler answers "unknown provider for
	// model" when no credential does. Neither reaches the provider.
	if strings.Contains(text, "auth_not_found") || strings.Contains(text, "unknown provider for model") {
		return Unsupported
	}
	// A local cooldown in CPA is not a provider response; retry later.
	if strings.Contains(text, "are cooling down") || strings.Contains(text, "auth_unavailable") {
		return Transient
	}
	if hardStatuses[host.StatusOf(err)] {
		return Hard
	}
	for _, marker := range []string{
		"http 400", "http 401", "http 403", "http 404", "http 409", "http 422", "http 429",
		"status 400", "status 401", "status 403", "status 404", "status 409", "status 422", "status 429",
		"not_found", "not found", "unsupported", "invalid model", "credential is unavailable",
	} {
		if strings.Contains(text, marker) {
			return Hard
		}
	}
	return Transient
}

// ModelRejected reports whether the provider refused the model itself, with
// HTTP 400 or 404. CPA lists a model for every credential of a provider, so a
// listed model can still be refused by one account. Authentication errors and
// rate limits concern the account and are not model rejections.
func ModelRejected(err error) bool {
	if Classify(err) != Hard || errors.Is(err, ErrNotConfirmed) || errors.Is(err, ErrNoModel) {
		return false
	}
	// A disabled account refuses every model, sometimes with HTTP 400.
	if failure.Diagnose(err).Reason == failure.ReasonSuspended {
		return false
	}
	if status := host.StatusOf(err); status != 0 {
		return status == 400 || status == 404
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"http 400", "http 404", "status 400", "status 404", "not_found", "not found", "invalid model"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
