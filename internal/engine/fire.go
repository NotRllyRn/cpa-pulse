package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/config"
	"github.com/NotRllyRn/cpa-pulse/internal/failure"
	"github.com/NotRllyRn/cpa-pulse/internal/host"
	"github.com/NotRllyRn/cpa-pulse/internal/ignite"
	"github.com/NotRllyRn/cpa-pulse/internal/models"
	"github.com/NotRllyRn/cpa-pulse/internal/notify"
	"github.com/NotRllyRn/cpa-pulse/internal/providers"
	"github.com/NotRllyRn/cpa-pulse/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// maxCandidates limits how many models one ignition tries.
const maxCandidates = 6

// TargetView is the ignition status of one quota group.
type TargetView struct {
	Windows             []TargetWindowStatus `json:"windows"`
	Key                 string               `json:"key"`
	Label               string               `json:"label"`
	Provider            string               `json:"provider"`
	AuthIndex           string               `json:"auth_index"`
	NextDue             time.Time            `json:"next_due"`
	Reset               time.Time            `json:"reset,omitempty"`
	Rolling             bool                 `json:"rolling"`
	LastSuccess         time.Time            `json:"last_success,omitempty"`
	LastAttempt         time.Time            `json:"last_attempt,omitempty"`
	LastModel           string               `json:"last_model,omitempty"`
	LastError           string               `json:"last_error,omitempty"`
	ConsecutiveFailures int                  `json:"consecutive_failures"`
	CircuitUntil        time.Time            `json:"circuit_until,omitempty"`
	CircuitReason       string               `json:"circuit_reason,omitempty"`
	// BlockedWindow names a used-up window that holds ignition back, and
	// BlockedUntil is its reset, zero when unknown.
	BlockedWindow string    `json:"blocked_window,omitempty"`
	BlockedUntil  time.Time `json:"blocked_until,omitempty"`
}

type target struct {
	group      GroupView
	cred       *Cred
	reset      time.Time
	source     string
	due        []WindowView
	confirmed  map[string]bool
	rollingDue map[string]bool
}

// groupIgnitable reports whether ignition is enabled for the group.
func groupIgnitable(cfg config.Config, provider, sourceLabel string) bool {
	if spec := providers.Lookup(provider); spec == nil || spec.Ignition == nil {
		return false
	}
	p := cfg.Provider(provider)
	if !p.Ignite {
		return false
	}
	if len(p.Groups) == 0 {
		return true
	}
	label := strings.ToLower(strings.TrimSpace(sourceLabel))
	for _, allowed := range p.Groups {
		if strings.ToLower(strings.TrimSpace(allowed)) == label {
			return true
		}
	}
	return false
}

// collectTargets returns groups with a supported request-to-quota mapping.
func (e *Engine) collectTargets(cfg config.Config) []target {
	return e.windowTargets(cfg, false)
}

func (e *Engine) windowTargets(cfg config.Config, includeDisabled bool) []target {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []target
	for _, group := range e.groups {
		cred := e.creds[group.AuthIndex]
		spec := providers.Lookup(group.Provider)
		if cred == nil || spec == nil || spec.Ignition == nil || (!includeDisabled && !groupIgnitable(cfg, group.Provider, group.SourceLabel)) {
			continue
		}
		recognized, supported := false, false
		for _, w := range group.Windows {
			recognized = recognized || quota.Class(w.Window) != ""
			supported = supported || providers.Pulseable(group.Provider, group.SourceLabel, w.Window)
		}
		if !recognized || (!includeDisabled && !supported) {
			continue
		}
		snapshot := *group
		snapshot.Windows = append([]WindowView(nil), group.Windows...)
		t := target{group: snapshot, cred: cred}
		if w, ok := group.fiveHour(); ok {
			t.reset, t.source = w.Reset, string(w.Source)
		}
		out = append(out, t)
	}
	sortTargets(out)
	return out
}

func sortTargets(targets []target) {
	for i := 1; i < len(targets); i++ {
		for j := i; j > 0 && targets[j].group.Label < targets[j-1].group.Label; j-- {
			targets[j], targets[j-1] = targets[j-1], targets[j]
		}
	}
}

func (e *Engine) nextDue(cfg config.Config) time.Time {
	if e.state == nil {
		return time.Time{}
	}
	now := e.now()
	var next time.Time
	for _, t := range e.collectTargets(cfg) {
		for _, w := range e.targetWindows(cfg, t, now) {
			if !w.NextDue.IsZero() && (next.IsZero() || w.NextDue.Before(next)) {
				next = w.NextDue
			}
		}
	}
	return next
}

// refreshTargets rebuilds the ignition status shown on the page.
func (e *Engine) refreshTargets(cfg config.Config) {
	if e.state == nil {
		return
	}
	now := e.now()
	targets := e.collectTargets(cfg)
	views := make([]TargetView, 0, len(targets))
	for _, t := range targets {
		ts := e.state.Target(t.group.Key)
		e.publishModelState(t.group.Key)
		view := TargetView{
			Key:                 t.group.Key,
			Label:               t.group.Label,
			Provider:            t.group.Provider,
			AuthIndex:           t.group.AuthIndex,
			Windows:             e.targetWindows(cfg, t, now),
			LastSuccess:         epochTime(ts.LastSuccessEpoch),
			LastAttempt:         epochTime(ts.LastAttemptEpoch),
			LastModel:           ts.LastModel,
			LastError:           ts.LastError,
			ConsecutiveFailures: ts.ConsecutiveFailures,
			CircuitReason:       ts.CircuitReason,
		}
		if until := epochTime(ts.CircuitOpenUntilEpoch); until.After(now) {
			view.CircuitUntil = until
		}
		if w, blocked := exhaustedWindow(t.group, now); blocked {
			view.BlockedWindow = quota.ShortLabel(w.Label)
			view.BlockedUntil = w.Reset
		}
		for _, w := range view.Windows {
			if !w.NextDue.IsZero() && (view.NextDue.IsZero() || w.NextDue.Before(view.NextDue)) {
				view.NextDue, view.Reset, view.Rolling = w.NextDue, w.Reset, w.Rolling
			}
		}
		views = append(views, view)
	}
	e.mu.Lock()
	e.targets = views
	e.mu.Unlock()
}

func epochTime(epoch float64) time.Time {
	if epoch <= 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(epoch*1e9)).UTC()
}

// performDue ignites every target whose due time has come.
func (e *Engine) performDue(ctx context.Context, cfg config.Config) bool {
	attempted := false
	for _, t := range e.collectTargets(cfg) {
		if ctx.Err() != nil {
			break
		}
		now := e.now()
		// A used-up window, such as the 7-day quota, rejects every request
		// until it resets; scheduled ignition skips the target until then.
		if _, blocked := exhaustedWindow(t.group, now); blocked {
			continue
		}
		// A rejected credential or a disabled account waits for a new
		// credential; see credentialBlocked.
		if e.credentialBlocked(t.cred.AuthIndex) {
			continue
		}
		t.due = e.dueWindows(cfg, t, now)
		if len(t.due) == 0 {
			continue
		}
		e.igniteTarget(ctx, cfg, t, false)
		attempted = true
	}
	return attempted
}

// configuredModel returns the model override for the target, if any.
func configuredModel(cfg config.Config, provider, groupLabel string) string {
	p := cfg.Provider(provider)
	for key, value := range p.Models {
		if strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(groupLabel)) && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return p.Model
}

// igniteTarget sends the ignition request, confirms it and records the
// outcome. It returns the error shown for a manual ignition.
func (e *Engine) igniteTarget(ctx context.Context, cfg config.Config, t target, manual bool) error {
	ts := e.state.Target(t.group.Key)
	schedule := e.schedule(cfg)
	start := e.now()
	ts.LastAttemptEpoch = float64(start.UnixNano()) / 1e9
	if t.due == nil {
		if manual {
			// A manual retry bypasses shared delays but must still confirm
			// each pending window from its own quota evidence.
			for _, w := range t.group.Windows {
				if !providers.Pulseable(t.group.Provider, t.group.SourceLabel, w.Window) {
					continue
				}
				ws := ts.IgnitionWindow(w.ID, quota.IsFiveHour(w.Window))
				if ws.RollingReset || (!w.Reset.IsZero() && !w.Reset.After(start)) || (w.Reset.IsZero() && quota.IsFiveHour(w.Window)) {
					t.due = append(t.due, w)
				}
			}
		} else {
			t.due = e.dueWindows(cfg, t, start)
		}
		if len(t.due) == 0 {
			t.due = nil
			for _, w := range t.group.Windows {
				if providers.Pulseable(t.group.Provider, t.group.SourceLabel, w.Window) && quota.IsFiveHour(w.Window) {
					t.due = append(t.due, w)
				}
			}
			if manual && len(t.due) == 0 {
				for _, w := range t.group.Windows {
					if providers.Pulseable(t.group.Provider, t.group.SourceLabel, w.Window) {
						t.due = append(t.due, w)
					}
				}
			}
		}
	}
	t.confirmed = map[string]bool{}
	t.rollingDue = map[string]bool{}
	for _, w := range t.due {
		ws := ts.IgnitionWindow(w.ID, quota.IsFiveHour(w.Window))
		t.rollingDue[w.ID] = ws.RollingReset
		ws.LastAttemptEpoch = start.Unix()
	}
	e.dirty = true
	// The fingerprint of the credential the request goes out with; a
	// rejection waits for another one. When it cannot be read, a rejection
	// keeps it unknown: the file read after the request may already hold a
	// new login the request did not use. See noteFingerprint.
	fingerprint := ""
	if !manual {
		fingerprint = e.credentialFingerprint(t.cred)
	}

	model, resp, err := e.execute(ctx, cfg, t)
	if err == nil {
		err = e.confirm(ctx, cfg, t, start, resp, manual)
	}
	now := e.now()
	if len(t.confirmed) > 0 {
		if len(t.due) > 0 && len(t.confirmed) == len(t.due) {
			ts.LastSuccessEpoch = float64(now.UnixNano()) / 1e9
		}
		ts.LastModel = model
		ts.LastSuccessfulModel, ts.ConfirmedModel = model, model
		e.publishModelState(t.group.Key)
	}
	if err != nil {
		d := failure.Diagnose(err)
		if manual {
			// Keep the automatic retry, circuit and credential state
			// unchanged. manual=1 without retry_seconds tells the page that
			// no retry follows.
			e.addEventDetail("warn", "ignite_failed", t.group.Key, t.group.Label,
				err.Error(), fmt.Sprintf("%s manual ignition failed: %v", t.group.Label, err),
				map[string]string{"manual": "1", "error": err.Error(), "summary": d.Summary})
			return err
		}
		if d.Reason == failure.ReasonAuth || d.Reason == failure.ReasonSuspended {
			// Retrying cannot help: ignition waits for a new credential or a
			// successful manual ignition.
			ignite.RecordCredentialFailure(ts, now, err, d.Summary)
			e.dirty = true
			wait := map[string]string{failure.ReasonAuth: "waiting for updated credentials", failure.ReasonSuspended: "account suspended"}[d.Reason]
			e.addEventDetail("error", "ignite_paused", t.group.Key, t.group.Label,
				fmt.Sprintf("Paused, %s: %s", wait, d.Summary),
				fmt.Sprintf("%s ignition failed; paused, %s: %v", t.group.Label, wait, err),
				map[string]string{"error": err.Error(), "summary": d.Summary, "reason": d.Reason})
			e.noteCredentialProblem(ctx, t.cred, t.group.Key, d, sourceIgnite, fingerprint)
			return err
		}
		outcome := schedule.RecordFailure(ts, now, err)
		e.dirty = true
		if outcome.CircuitOpened {
			until := outcome.Until.In(cfg.Location()).Format("01/02 15:04")
			params := map[string]string{"until": eventTime(outcome.Until), "error": err.Error(), "summary": d.Summary}
			if d.Reason != "" {
				params["reason"] = d.Reason
			}
			e.addEventDetail("error", "ignite_paused", t.group.Key, t.group.Label,
				fmt.Sprintf("Paused until %s: %s", until, d.Summary),
				fmt.Sprintf("%s ignition failed; paused until %s: %v", t.group.Label, until, err), params)
			if d.Reason == failure.ReasonRateLimited && outcome.NotifyCircuit {
				e.credentialEvent(t.cred, t.group.Key, d, sourceIgnite, outcome.Until)
			}
			if outcome.NotifyCircuit && e.alerts != nil && e.alerts.Sender != nil {
				msg := notify.CircuitMessage(e.language(), t.group.Label, outcome.Until, d.Summary, cfg.Location())
				if d.Reason == failure.ReasonRateLimited {
					msg = rateLimitedNotice(e.language(), t.group.Label, d, outcome.Until, cfg.Location())
				}
				switch sendErr := e.alerts.Sender.Send(ctx, msg); {
				case errors.Is(sendErr, notify.ErrNotConfigured):
					// Notifications are off.
				case sendErr != nil:
					e.notifyFailed(t.group.Key, msg, sendErr)
				default:
					e.notified(t.group.Key, msg)
				}
			}
		} else {
			retry := "Retry in " + retryText(outcome.RetryIn)
			if outcome.Cooldown {
				retry = "CPA cooldown; " + strings.ToLower(retry)
			}
			params := map[string]string{"retry_seconds": strconv.Itoa(int(outcome.RetryIn.Round(time.Second).Seconds())), "error": err.Error(), "summary": d.Summary}
			if outcome.Cooldown {
				params["cooldown"] = "1"
			}
			e.addEventDetail("warn", "ignite_failed", t.group.Key, t.group.Label,
				fmt.Sprintf("%s: %v", retry, err),
				fmt.Sprintf("%s ignition failed; %s: %v", t.group.Label, strings.ToLower(retry), err), params)
		}
		return err
	}
	ignite.RecordSuccess(ts, model, now)
	ts.LastSuccessfulModel, ts.ConfirmedModel = model, model
	e.publishModelState(t.group.Key)
	e.dirty = true
	e.noteCredentialOK(ctx, t.cred, sourceIgnite)
	resetText := "unknown"
	params := map[string]string{"model": model}
	if group, ok := e.group(t.group.Key); ok {
		reset := time.Time{}
		if w, ok := group.fiveHour(); ok {
			reset = w.Reset
		}
		if reset.IsZero() {
			for _, w := range group.Windows {
				if t.confirmed[w.ID] && !w.Reset.IsZero() {
					reset = w.Reset
					break
				}
			}
		}
		if !reset.IsZero() {
			resetText = reset.In(cfg.Location()).Format("01/02 15:04:05")
			params["reset"] = eventTime(reset)
		}
	}
	kind := "ignite"
	if manual {
		kind = "ignite_manual"
	}
	e.addEventDetail("info", kind, t.group.Key, t.group.Label,
		fmt.Sprintf("%s · next reset %s", model, resetText),
		fmt.Sprintf("%s ignition succeeded (%s); next reset %s", t.group.Label, model, resetText), params)
	return nil
}

func (e *Engine) group(key string) (GroupView, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	group, ok := e.groups[key]
	if !ok {
		return GroupView{}, false
	}
	snapshot := *group
	snapshot.Windows = append([]WindowView(nil), group.Windows...)
	return snapshot, true
}

// execute tries compatible models in price order, with at most two upstream
// attempts. Manual overrides use one model; local routing failures may advance.
func (e *Engine) execute(ctx context.Context, cfg config.Config, t target) (string, pluginapi.HostModelExecutionResponse, error) {
	var empty pluginapi.HostModelExecutionResponse
	spec := providers.Lookup(t.group.Provider)
	override := manualModel(cfg, t)
	ts := e.state.Target(t.group.Key)
	e.noteModelFingerprint(t.cred, e.credentialFingerprint(t.cred))
	defer e.publishModelState(t.group.Key)

	available, err := e.lister.List(ctx, cfg.CPABaseURL, cfg.ModelsAPIKey)
	e.noteModels(available, err)
	var candidates []string
	switch {
	case err != nil && override != "":
		candidates = []string{override}
	case err != nil && ts.LastModel != "":
		// The last working model remains a fallback while CPA's list is unavailable.
		candidates = rankedModels(e.priceManager(), spec, t.group.SourceLabel, []string{ts.LastModel}, ts.ConfirmedModel)
		if len(candidates) == 0 {
			return "", empty, fmt.Errorf("%w: no eligible model", ignite.ErrNoModel)
		}
	case err != nil:
		return "", empty, err
	default:
		if override != "" {
			candidates, err = providers.Candidates(spec, t.group.SourceLabel, override, available)
		} else {
			candidates = rankedModels(e.priceManager(), spec, t.group.SourceLabel, available, ts.ConfirmedModel)
		}
		if err != nil {
			return "", empty, fmt.Errorf("%w: %v", ignite.ErrNoModel, err)
		}
		if len(candidates) == 0 {
			return "", empty, fmt.Errorf("%w: no eligible model", ignite.ErrNoModel)
		}
	}
	if override == "" {
		candidates = e.skipRefused(t.group.Key, candidates)
	}
	if len(candidates) == 0 {
		return "", empty, fmt.Errorf("%w: no eligible model; retry after cooldown", ignite.ErrNoModel)
	}
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}

	var lastErr error
	upstream := 0
	refused := ""
	refusedStatus := 0
	for _, model := range candidates {
		if ctx.Err() != nil || e.stopping() {
			return model, empty, context.Canceled
		}
		protocol, body, err := providers.Request(spec, model)
		if err != nil {
			return "", empty, err
		}
		forced := t.cred.HostProvider
		if forced == "" {
			forced = t.group.Provider
		}
		resp, err := e.executeStream(ctx, t, pluginapi.HostModelExecutionRequest{
			EntryProtocol: protocol, ExitProtocol: protocol, Model: model,
			Stream: true, Body: body, ForcedProvider: forced, AuthID: t.cred.ID,
		}, ignitionOutputWindow)
		if err == nil {
			if refused != "" {
				e.noteRefused(t, refused, model)
			}
			return model, resp, nil
		}
		lastErr = err
		if override == "" && localModelUnavailable(err) {
			continue
		}
		upstream++
		if override == "" && modelRejected(err) {
			e.rememberRefused(t, model)
			if upstream < 2 {
				refused, refusedStatus = model, host.StatusOf(err)
				continue
			}
		}
		if refused != "" {
			err = fmt.Errorf("%w; earlier %s was rejected by the provider (status %d)", err, refused, refusedStatus)
		}
		return model, resp, err
	}
	return "", empty, fmt.Errorf("%w (last attempt: %v)", ignite.ErrNoModel, lastErr)
}

// Only CPA's local model-routing failures can advance without an upstream attempt.
func localModelUnavailable(err error) bool {
	d := failure.Diagnose(err)
	if d.Reason != "" || d.Status == 401 || d.Status == 403 || d.Status == 429 {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "auth_not_found") || strings.Contains(text, "unknown provider for model")
}

func modelRejected(err error) bool {
	d := failure.Diagnose(err)
	if d.Reason != "" || d.Status == 401 || d.Status == 429 {
		return false
	}
	text := strings.ToLower(err.Error())
	explicit := false
	for _, marker := range []string{"model_not_found", "model_not_supported", "unsupported_model", "invalid_model", "model_access_denied", "model_entitlement"} {
		if strings.Contains(text, marker) {
			explicit = true
		}
	}
	if d.Status == 404 && strings.Contains(text, "not_found_error") {
		return true
	}
	if d.Status != 0 && d.Status != 400 && d.Status != 403 && d.Status != 404 {
		return false
	}
	message := err.Error()
	var hostErr *host.Error
	if errors.As(err, &hostErr) {
		message = hostErr.Message
	}
	return explicit || modelAccessMessage(strings.ToLower(message))
}

func modelAccessMessage(text string) bool {
	if !strings.Contains(text, "model") {
		return false
	}
	for _, marker := range []string{"not found", "not supported", "unsupported", "does not exist", "do not have access", "does not have access", "no access", "not entitled", "not available", "permission to access"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// refusedFor is how long a model the provider refused for a quota group is
// skipped. A new model can reach accounts in stages, so it is tried again
// after a day instead of being dropped; a day keeps requests for a model the
// account cannot use rare.
const refusedFor = 24 * time.Hour

// noteRefused remembers that the provider refused model for the target while
// fallback worked, which shows the refusal concerns the model.
func (e *Engine) noteRefused(t target, model, fallback string) {
	e.rememberRefused(t, model)
	hours := strconv.Itoa(int(refusedFor / time.Hour))
	e.addEventDetail("warn", "model_refused", t.group.Key, t.group.Label,
		fmt.Sprintf("%s was rejected by the provider; using %s and skipping it for %s hours", model, fallback, hours),
		fmt.Sprintf("%s ignition model %s was rejected by the provider; using %s and skipping it for %s hours", t.group.Label, model, fallback, hours),
		map[string]string{"model": model, "fallback": fallback, "hours": hours})
}

func (e *Engine) rememberRefused(t target, model string) {
	if e.refused == nil {
		e.refused = map[string]time.Time{}
	}
	e.refused[t.group.Key+"\x00"+model] = e.now()
	e.state.RefusedModels = e.refused
	e.dirty = true
}

// skipRefused drops refused candidates and forgets expired refusals.
func (e *Engine) skipRefused(key string, candidates []string) []string {
	now := e.now()
	for k, at := range e.refused {
		if now.Sub(at) >= refusedFor {
			delete(e.refused, k)
			e.dirty = true
		}
	}
	var out []string
	for _, model := range candidates {
		if _, ok := e.refused[key+"\x00"+model]; !ok {
			out = append(out, model)
		}
	}
	return out
}

// maybeReadModels reads the CPA model list after a start and after a change
// of the CPA address or models key, so that the settings show the next
// automatic model without waiting for an ignition. Otherwise the list is only
// read for an ignition. A failed read is retried with the next poll.
func (e *Engine) maybeReadModels(ctx context.Context, cfg config.Config, pollDue bool) {
	source := ""
	if cfg.ModelsAPIKey != "" {
		source = cfg.CPABaseURL + "\x00" + cfg.ModelsAPIKey
	}
	if source != e.modelsSource {
		// Models and errors of another CPA or key do not describe the next
		// ignition.
		e.modelsSource, e.modelsTried, e.modelsOK = source, false, false
		e.mu.Lock()
		e.nextModels, e.modelsErr, e.availableModels = nil, "", nil
		e.mu.Unlock()
	}
	if source == "" || e.modelsOK || (e.modelsTried && !pollDue) {
		return
	}
	e.modelsTried = true
	available, err := e.lister.List(ctx, cfg.CPABaseURL, cfg.ModelsAPIKey)
	e.noteModels(available, err)
	e.modelsOK = err == nil
}

// noteModels records the result of reading the model list and the first
// automatic candidate of each provider. A failed read keeps the last known
// models.
func (e *Engine) noteModels(available []string, err error) {
	available = models.Unique(available)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.modelsErr = err.Error()
		return
	}
	e.modelsErr = ""
	e.availableModels = append([]string{}, available...)
	next := map[string]string{}
	for _, p := range providers.All() {
		if p.Ignition == nil {
			continue
		}
		if candidates := rankedModels(e.pricingMgr, p, p.Ignition.HintGroup, available, ""); len(candidates) > 0 {
			next[p.ID] = candidates[0]
		}
	}
	e.nextModels = next
}

func truncateText(text string, n int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n])
}

// confirmed reports whether the target's 5-hour window has a fixed future
// reset observed after start.
func (e *Engine) confirmed(key string, start, now time.Time) bool {
	group, ok := e.group(key)
	if !ok {
		return false
	}
	w, ok := group.fiveHour()
	if !ok || w.ObservedAt.Before(start) || w.Reset.IsZero() || !w.Reset.After(now) {
		return false
	}
	return !e.state.Target(key).IgnitionWindow(w.ID, true).RollingReset
}

// confirm checks that the ignition started a window. Claude responses carry
// the new reset in their headers; Codex reports it through the usage record
// of the same request. When neither arrives, and for providers without quota
// in their headers, the credential is queried actively. The queries of a
// manual ignition leave the credential state alone, like its failure.
func (e *Engine) confirm(ctx context.Context, cfg config.Config, t target, start time.Time, resp pluginapi.HostModelExecutionResponse, manual bool) error {
	var groups []quota.RawGroup
	if spec := providers.Lookup(t.group.Provider); spec != nil && spec.Headers != nil {
		groups = spec.Headers(resp.Headers, e.now())
	}
	if len(groups) > 0 {
		now := e.now()
		e.passiveAt[t.cred.AuthIndex] = now
		e.notePassiveFable(t.cred, groups)
		e.applyGroups(ctx, cfg, t.cred, groups, quota.SourcePassive, now, start, true)
	}
	if e.confirmWindows(t, start, e.now(), manual) {
		return nil
	}
	for _, delay := range confirmDelays {
		if !e.waitWithUsage(ctx, delay) {
			return context.Canceled
		}
		if e.confirmWindows(t, start, e.now(), manual) {
			return nil
		}
		if err := e.queryCred(ctx, cfg, t.cred, !manual); err != nil {
			e.logf("warn", "Quota refresh failed during ignition confirmation: %s: %v", t.group.Label, err)
		}
		if e.confirmWindows(t, start, e.now(), manual) {
			return nil
		}
	}
	return ignite.ErrNotConfirmed
}

// retryText formats a retry delay in seconds below one minute, in minutes
// otherwise.
func retryText(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Round(time.Second).Seconds()))
	}
	return fmt.Sprintf("%d min", int(d.Round(time.Minute).Minutes()))
}
