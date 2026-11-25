// Package store persists cpa-pulse state and quota history in the plugin
// data directory.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// WindowState is the notification baseline of one quota window.
type WindowState struct {
	Severity         string  `json:"severity,omitempty"`
	NotifiedSeverity string  `json:"notified_severity,omitempty"`
	Remaining        float64 `json:"remaining"`
	Reset            string  `json:"reset,omitempty"`
	LastSeen         int64   `json:"last_seen,omitempty"`
	ResetNotice1hFor string  `json:"reset_notice_1h_for,omitempty"`
	ResetNotice1dFor string  `json:"reset_notice_1d_for,omitempty"`
	// Exhausted is set when the window was seen used up in its current cycle.
	Exhausted bool `json:"exhausted,omitempty"`
	// Long is set once the window was seen more than a day before its
	// reset, which makes a window of unknown kind count as a 7-day window.
	Long bool `json:"long,omitempty"`
	// CycleReset, CycleRemaining and CycleSeen are the last reading taken
	// before the reset time it reported, kept to tell what was left when
	// that cycle ended.
	CycleReset     string  `json:"cycle_reset,omitempty"`
	CycleRemaining float64 `json:"cycle_remaining,omitempty"`
	CycleSeen      int64   `json:"cycle_seen,omitempty"`
	// EndedReset is the reset time of the last cycle that ended. Readings
	// that still report it arrive late and are ignored.
	EndedReset string `json:"ended_reset,omitempty"`
	// BounceSince and BounceReadings count the readings in a row that report
	// EndedReset as a future reset, from when the first one was seen, in Unix
	// seconds; see notify.Bounced.
	BounceSince    int64 `json:"bounce_since,omitempty"`
	BounceReadings int   `json:"bounce_readings,omitempty"`
	// PendingRecovery is a recovery notification not delivered yet.
	PendingRecovery *PendingRecovery `json:"pending_recovery,omitempty"`
	// Rebase marks a plan change or a window absent from an active query.
	// Its first returning reading only records a baseline.
	// GroupState.ActiveWindows controls whether passive updates are allowed.
	Rebase bool `json:"rebase,omitempty"`
}

// PendingRecovery is a detected reset of one window, kept until its
// notification is delivered.
type PendingRecovery struct {
	// At is when the reset was detected, in Unix seconds.
	At int64 `json:"at"`
	// Previous is the last remaining percentage of the ended cycle, or nil
	// when no reading close enough to the reset exists.
	Previous *float64 `json:"previous,omitempty"`
	// Exhausted is set when the ended cycle was used up.
	Exhausted bool `json:"exhausted,omitempty"`
}

// GroupState holds the notification baselines of one quota group.
type GroupState struct {
	// ActiveWindows lists windows confirmed by the last successful active query; nil means unconfirmed.
	ActiveWindows []string `json:"active_windows"`
	// LegacyUnconfirmed restricts loaded groups without reliable active confirmation; it is not persisted.
	LegacyUnconfirmed bool                    `json:"-"`
	Label             string                  `json:"label,omitempty"`
	LastSeen          int64                   `json:"last_seen,omitempty"`
	Windows           map[string]*WindowState `json:"windows"`
}

// Observation is the last observed 5-hour reset of an ignition target.
type Observation struct {
	SeenEpoch  float64  `json:"seen_epoch"`
	ResetEpoch *float64 `json:"reset_epoch,omitempty"`
}

// TargetState is the ignition schedule and failure protection state of one
// ignition target.
type TargetState struct {
	Windows                   map[string]*IgnitionWindowState `json:"windows,omitempty"`
	LastSuccessfulModel       string                          `json:"last_successful_model,omitempty"`
	ConfirmedModel            string                          `json:"confirmed_model,omitempty"`
	RollingReset              bool                            `json:"rolling_reset,omitempty"`
	ResetObservation          *Observation                    `json:"reset_observation,omitempty"`
	LastSuccessEpoch          float64                         `json:"last_success_epoch,omitempty"`
	LastAttemptEpoch          float64                         `json:"last_attempt_epoch,omitempty"`
	LastModel                 string                          `json:"last_model,omitempty"`
	LastError                 string                          `json:"last_error,omitempty"`
	LastFailureEpoch          float64                         `json:"last_failure_epoch,omitempty"`
	ConsecutiveFailures       int                             `json:"consecutive_failures,omitempty"`
	RetryAtEpoch              float64                         `json:"retry_at_epoch,omitempty"`
	CircuitOpenUntilEpoch     float64                         `json:"circuit_open_until_epoch,omitempty"`
	CircuitReason             string                          `json:"circuit_reason,omitempty"`
	CircuitNotifiedUntilEpoch float64                         `json:"circuit_notified_until_epoch,omitempty"`
}

// SentEvent is the last notification sent for one scheduled reset.
type SentEvent struct {
	Key string `json:"key"`
	// AnnouncedAt is the announcedAt of the sent post, in Unix seconds.
	AnnouncedAt int64 `json:"announced_at,omitempty"`
	// Content is the notification body in UTC and English, used to tell
	// whether a later post changes anything.
	Content string `json:"content"`
}

// CodexResetState deduplicates Did Codex Reset records.
type CodexResetState struct {
	Initialized bool     `json:"initialized,omitempty"`
	SeenKeys    []string `json:"seen_keys,omitempty"`
	// SentEvents lists the scheduled resets already notified, newest first.
	SentEvents     []SentEvent `json:"sent_events,omitempty"`
	LastCheckEpoch int64       `json:"last_check_epoch,omitempty"`
}

// State is the content of state.json.
type State struct {
	RefusedModels     map[string]time.Time    `json:"refused_models,omitempty"`
	ModelFingerprints map[string]string       `json:"model_fingerprints,omitempty"`
	Version           int                     `json:"version"`
	Groups            map[string]*GroupState  `json:"groups"`
	Scheduler         map[string]*TargetState `json:"scheduler"`
	CodexReset        *CodexResetState        `json:"codex_reset_updates,omitempty"`
	// CooldownNotices maps an auth index to the end of the stale CPA
	// cooldown that was already notified, in Unix seconds.
	CooldownNotices map[string]int64 `json:"cooldown_notices,omitempty"`
	// Language retains the reported notification language for compatibility.
	// Notifications always use English.
	Language string `json:"language,omitempty"`
	// Plans maps an auth index to the plan last read for the account.
	Plans map[string]*PlanState `json:"plans,omitempty"`
	// Credentials maps an auth index to the credential problem of the
	// account, see CredentialState.
	Credentials map[string]*CredentialState `json:"credentials,omitempty"`
}

// CredentialState is a credential problem of one account: the provider
// rejected the credential (reason "auth") or disabled the account
// ("suspended"). Ignition stays paused while it lasts, and it is notified
// once. See internal/engine/credential.go.
type CredentialState struct {
	// Reason is "auth" or "suspended", empty when the account has no
	// problem.
	Reason    string `json:"reason,omitempty"`
	Status    string `json:"status,omitempty"`
	Code      string `json:"code,omitempty"`
	Summary   string `json:"summary,omitempty"`
	AppealURL string `json:"appeal_url,omitempty"`
	// Source is "ignite" or "query", whichever found the problem, and
	// Group the quota group key of its event, which its notification
	// carries too.
	Source string `json:"source,omitempty"`
	Group  string `json:"group,omitempty"`
	// Since is when the problem was found, in Unix seconds.
	Since int64 `json:"since,omitempty"`
	// Fingerprint identifies the credential the problem was found with; a
	// different one, after logging in again, lets ignition try again.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Changed is set when the credential differs from Fingerprint.
	Changed bool `json:"changed,omitempty"`
	// Ignited is set when an ignition failed with a problem of this
	// credential, also one a query found first. Only a successful ignition
	// ends such a problem, not a working query; a problem of another reason
	// that replaces it keeps the flag until the credential changes.
	Ignited bool `json:"ignited,omitempty"`
	// Notice is the notification still to send: "problem" or "restored",
	// empty when there is none. NoticeAt is when it became due, and
	// Notified tells whether the problem notification was delivered.
	Notice   string `json:"notice,omitempty"`
	NoticeAt int64  `json:"notice_at,omitempty"`
	Notified bool   `json:"notified,omitempty"`
	// QueryReason, QuerySince and QueryCount count the quota queries in a
	// row that failed with a credential problem not confirmed yet.
	QueryReason string `json:"query_reason,omitempty"`
	QuerySince  int64  `json:"query_since,omitempty"`
	QueryCount  int    `json:"query_count,omitempty"`
}

// Credential returns the credential state of authIndex, creating it when
// missing.
func (s *State) Credential(authIndex string) *CredentialState {
	if s.Credentials == nil {
		s.Credentials = map[string]*CredentialState{}
	}
	c, ok := s.Credentials[authIndex]
	if !ok || c == nil {
		c = &CredentialState{}
		s.Credentials[authIndex] = c
	}
	return c
}

// PlanState is the raw plan value of one account, see providers.PlanOf.
type PlanState struct {
	Plan string `json:"plan,omitempty"`
	// Checked is when the plan was last requested on its own, in Unix
	// seconds, for providers whose usage response does not state it. A
	// failed recheck is kept earlier than it was sent, so that the next
	// request follows sooner than a day later.
	Checked int64 `json:"checked,omitempty"`
	// Windows lists the quota windows of the last active query.
	Windows string `json:"windows,omitempty"`
	// RecheckUntil is when the plan stops being requested with every active
	// query after the windows changed, in Unix seconds.
	RecheckUntil int64 `json:"recheck_until,omitempty"`
	// Notice is a plan change notification not delivered yet.
	Notice *PlanNotice `json:"notice,omitempty"`
}

// PlanNotice is a plan change, kept until its notification is delivered.
type PlanNotice struct {
	From string `json:"from"`
	To   string `json:"to"`
	// At is when the change was seen, in Unix seconds.
	At int64 `json:"at"`
}

// PlanFor returns the plan state of authIndex, creating it when missing.
func (s *State) PlanFor(authIndex string) *PlanState {
	if s.Plans == nil {
		s.Plans = map[string]*PlanState{}
	}
	plan, ok := s.Plans[authIndex]
	if !ok || plan == nil {
		plan = &PlanState{}
		s.Plans[authIndex] = plan
	}
	return plan
}

// CooldownNotice records that the stale cooldown of authIndex ending at
// until was notified.
func (s *State) CooldownNotice(authIndex string, until int64) {
	if s.CooldownNotices == nil {
		s.CooldownNotices = map[string]int64{}
	}
	s.CooldownNotices[authIndex] = until
}

// NewState returns an empty state.
func NewState() *State {
	return &State{
		Version:   1,
		Groups:    map[string]*GroupState{},
		Scheduler: map[string]*TargetState{},
	}
}

func (s *State) ensure() {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Groups == nil {
		s.Groups = map[string]*GroupState{}
	}
	if s.Scheduler == nil {
		s.Scheduler = map[string]*TargetState{}
	}
	for key, group := range s.Groups {
		if group == nil {
			s.Groups[key] = &GroupState{Windows: map[string]*WindowState{}}
		} else if group.Windows == nil {
			group.Windows = map[string]*WindowState{}
		}
	}
}

// Group returns the state of group key, creating it when missing.
func (s *State) Group(key string) *GroupState {
	s.ensure()
	group, ok := s.Groups[key]
	if !ok {
		group = &GroupState{Windows: map[string]*WindowState{}}
		s.Groups[key] = group
	}
	return group
}

// Target returns the state of ignition target id, creating it when missing.
func (s *State) Target(id string) *TargetState {
	s.ensure()
	target, ok := s.Scheduler[id]
	if !ok || target == nil {
		target = &TargetState{}
		s.Scheduler[id] = target
	}
	return target
}

// StateFile reads and writes state.json. Writes go to a temporary file that
// replaces the old file, so a crash never leaves a partly written state.
type StateFile struct {
	Path string
	Now  func() time.Time
	mu   sync.Mutex
}

// Load reads the state. A missing file yields an empty state and fresh=true.
func (f *StateFile) Load() (state *State, fresh bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, err := os.ReadFile(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewState(), true, nil
	}
	if err != nil {
		return NewState(), true, err
	}
	state = NewState()
	if err := json.Unmarshal(raw, state); err != nil {
		return NewState(), true, fmt.Errorf("state file is corrupt; rebuilt empty state: %w", err)
	}
	state.ensure()
	f.pruneRefused(state)
	state.restoreActiveWindows()
	return state, false, nil
}

// restoreActiveWindows trusts only nonempty plans.windows from successful
// active queries. Notification baselines, Rebase and ignition observations
// cannot confirm that a window still exists.
func (s *State) restoreActiveWindows() {
	for key, group := range s.Groups {
		if group.ActiveWindows != nil {
			continue
		}
		group.LegacyUnconfirmed = true
		parts := strings.SplitN(key, ":", 3)
		if len(parts) != 3 {
			continue
		}
		plan := s.Plans[parts[1]]
		if plan == nil || plan.Windows == "" {
			continue
		}
		windows := []string{}
		valid := true
		for _, entry := range strings.Split(plan.Windows, "\n") {
			name, id, ok := strings.Cut(entry, "|")
			if !ok || name == "" || id == "" || strings.Contains(id, "|") {
				valid = false
				break
			}
			if name == parts[2] {
				windows = append(windows, id)
			}
		}
		if valid {
			group.ActiveWindows = windows
			group.LegacyUnconfirmed = false
			for id, ws := range group.Windows {
				if ws != nil && !slices.Contains(windows, id) {
					ws.Rebase = true
				}
			}
		}
	}
}

// Save writes state atomically.
func (f *StateFile) Save(state *State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneRefused(state)
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}

// ResetObservation is the last observed reset of an ignition window.
type ResetObservation = Observation

// IgnitionWindowState records scheduling progress for one provider window ID.
type IgnitionWindowState struct {
	RollingReset     bool              `json:"rolling_reset,omitempty"`
	ResetObservation *ResetObservation `json:"reset_observation,omitempty"`
	LastSuccessEpoch int64             `json:"last_success_epoch,omitempty"`
	LastAttemptEpoch int64             `json:"last_attempt_epoch,omitempty"`
	PulsedResetEpoch int64             `json:"pulsed_reset_epoch,omitempty"`
}

// IgnitionWindow returns state for an observed window. Only a 5h window can
// inherit the legacy target schedule; existing window entries take precedence.
func (t *TargetState) IgnitionWindow(id string, fiveHour bool) *IgnitionWindowState {
	if t.Windows == nil {
		t.Windows = map[string]*IgnitionWindowState{}
	}
	w := t.Windows[id]
	if w == nil {
		w = &IgnitionWindowState{}
		if fiveHour {
			w.RollingReset, w.ResetObservation = t.RollingReset, t.ResetObservation
			w.LastSuccessEpoch, w.LastAttemptEpoch = int64(t.LastSuccessEpoch), int64(t.LastAttemptEpoch)
		}
		t.Windows[id] = w
	}
	if fiveHour {
		t.RollingReset, t.ResetObservation = false, nil
	}
	return w
}

func (f *StateFile) pruneRefused(state *State) {
	now := time.Now()
	if f.Now != nil {
		now = f.Now()
	}
	for key, at := range state.RefusedModels {
		if now.Sub(at) >= 24*time.Hour {
			delete(state.RefusedModels, key)
		}
	}
}
