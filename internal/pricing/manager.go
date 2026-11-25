package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Option func(*Manager)

func WithHTTPClient(client *http.Client) Option {
	return func(m *Manager) {
		if client != nil {
			m.client = client
		}
	}
}

func WithURL(url string) Option { return func(m *Manager) { m.url = url } }

type refreshCall struct {
	done chan struct{}
	err  error
}

// Manager keeps source prices and local overrides separate. All returned values
// are copies and are safe for callers to edit.
type Manager struct {
	mu               sync.RWMutex
	dataDir          string
	client           *http.Client
	url              string
	snapshot         CatalogSnapshot
	overrides        map[string]Override
	flight           *refreshCall
	catalog          []Entry
	lookup           map[string]Entry
	overrideWriteErr error
}

// NewManager loads disk data without network access. Missing files are normal;
// invalid files leave the fallback intact and are reported in Snapshot.LastError.
func NewManager(dataDir string, options ...Option) *Manager {
	m := &Manager{
		dataDir:   dataDir,
		client:    &http.Client{Timeout: 15 * time.Second},
		url:       ModelsDevURL,
		snapshot:  CatalogSnapshot{Version: Version, Source: "embedded", Entries: EmbeddedEntries()},
		overrides: make(map[string]Override),
	}
	for _, option := range options {
		option(m)
	}
	var warnings []string
	var snapshot CatalogSnapshot
	if err := readJSON(filepath.Join(dataDir, "model_prices.json"), &snapshot); err == nil {
		if err = validateSnapshot(snapshot); err == nil && snapshot.Source != "embedded" {
			m.snapshot = snapshot
		} else if err != nil {
			warnings = append(warnings, "load model prices: "+err.Error())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		warnings = append(warnings, "load model prices: "+err.Error())
	}
	var overrides OverrideFile
	overridePath := filepath.Join(dataDir, "model_price_overrides.json")
	err := readJSON(overridePath, &overrides)
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	corrupt := errors.As(err, &syntaxErr) || errors.As(err, &typeErr)
	if err == nil {
		err = validateOverrides(overrides)
		corrupt = err != nil
	}
	if err == nil {
		for key, ov := range overrides.Overrides {
			m.overrides[key] = cloneOverride(ov)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		warnings = append(warnings, "load price overrides: "+err.Error())
		m.overrideWriteErr = err
		if corrupt {
			if backupErr := quarantineOverrides(overridePath); backupErr != nil {
				m.overrideWriteErr = fmt.Errorf("preserve corrupt price overrides: %w", backupErr)
				warnings = append(warnings, m.overrideWriteErr.Error())
			} else {
				m.overrideWriteErr = nil
			}
		}
	}
	if len(warnings) > 0 {
		m.snapshot.LastError = strings.Join(warnings, "; ")
	}
	m.rebuildCatalogLocked()
	return m
}

// Snapshot returns the source catalog before overrides.
func (m *Manager) Snapshot() CatalogSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneSnapshot(m.snapshot)
}

func cloneSnapshot(s CatalogSnapshot) CatalogSnapshot {
	s.Entries = append([]Entry(nil), s.Entries...)
	return s
}

func cloneOverride(ov Override) Override {
	if ov.Price != nil {
		p := *ov.Price
		ov.Price = &p
	}
	return ov
}

func (m *Manager) Overrides() OverrideFile {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := OverrideFile{Version: Version, Overrides: make(map[string]Override, len(m.overrides))}
	for key, ov := range m.overrides {
		out.Overrides[key] = cloneOverride(ov)
	}
	return out
}

// Catalog returns effective entries, including tombstones with Disabled set.
func (m *Manager) Catalog() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Entry(nil), m.catalog...)
}

func (m *Manager) rebuildCatalogLocked() {
	entries := make(map[string]Entry, len(m.snapshot.Entries)+len(m.overrides))
	for _, e := range m.snapshot.Entries {
		entries[e.Provider+"/"+e.Model] = e
	}
	for key, ov := range m.overrides {
		provider, model, _ := strings.Cut(key, "/")
		matched := false
		for entryKey, e := range entries {
			if e.Provider != provider || canonicalModel(provider, e.Model) != model {
				continue
			}
			entries[entryKey] = applyOverride(e, ov)
			matched = true
		}
		if !matched {
			e := Entry{Provider: provider, Model: model, TextGeneration: ov.Price != nil, Source: "override"}
			entries[key] = applyOverride(e, ov)
		}
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	sortEntries(out)
	m.catalog, m.lookup = out, entries
}

func applyOverride(e Entry, ov Override) Entry {
	e.Disabled = ov.Disabled
	if ov.Price != nil {
		e.Price = *ov.Price
		e.HasPrice = true
		e.Custom = true
		e.Source = "override"
	}
	return e
}

// Refresh coalesces overlapping requests. A waiting caller can cancel its wait;
// the caller that starts the fetch owns that fetch's context.
func (m *Manager) Refresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if call := m.flight; call != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-call.done:
			return call.err
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	m.flight = call
	previous := cloneSnapshot(m.snapshot)
	m.mu.Unlock()
	next, err := m.fetch(ctx, previous)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		next = previous
		next.CheckedAt = time.Now().UTC()
		next.LastError = err.Error()
	}
	if next.Source != "embedded" {
		if saveErr := writeJSON(filepath.Join(m.dataDir, "model_prices.json"), next); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("save model prices: %w", saveErr))
			next.LastError = err.Error()
		}
	}
	m.snapshot = next
	m.rebuildCatalogLocked()
	call.err = err
	m.flight = nil
	close(call.done)
	return err
}

// NeedsRefresh uses a one-hour error backoff. Out-of-range intervals use 24 hours;
// zero disables automatic refresh. Embedded data needs an initial refresh.
func (m *Manager) NeedsRefresh(now time.Time, refreshHours int) bool {
	if refreshHours == 0 {
		return false
	}
	if refreshHours < 1 || refreshHours > 168 {
		refreshHours = DefaultRefreshHours
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.snapshot.LastError != "" && !m.snapshot.CheckedAt.IsZero() && now.Sub(m.snapshot.CheckedAt) < time.Hour {
		return false
	}
	lastSuccess := m.snapshot.FetchedAt
	if m.snapshot.LastSuccessCheckAt.After(lastSuccess) {
		lastSuccess = m.snapshot.LastSuccessCheckAt
	}
	return lastSuccess.IsZero() || now.Sub(lastSuccess) >= time.Duration(refreshHours)*time.Hour
}

func (m *Manager) SetOverride(provider, model string, ov Override) error {
	if err := validateIdentity(provider, model); err != nil {
		return err
	}
	if ov.Price != nil {
		if err := ov.Price.Validate(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.overrideWriteErr != nil {
		return m.overrideWriteErr
	}
	key := overrideKey(provider, model)
	next := make(map[string]Override, len(m.overrides)+1)
	for k, v := range m.overrides {
		next[k] = v
	}
	next[key] = cloneOverride(ov)
	if err := writeJSON(filepath.Join(m.dataDir, "model_price_overrides.json"), OverrideFile{Version: Version, Overrides: next}); err != nil {
		return fmt.Errorf("save price overrides: %w", err)
	}
	m.overrides = next
	m.rebuildCatalogLocked()
	return nil
}

func (m *Manager) DeleteOverride(provider, model string) error {
	if err := validateIdentity(provider, model); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.overrideWriteErr != nil {
		return m.overrideWriteErr
	}
	key := overrideKey(provider, model)
	next := make(map[string]Override, len(m.overrides))
	for k, v := range m.overrides {
		if k != key {
			next[k] = v
		}
	}
	if err := writeJSON(filepath.Join(m.dataDir, "model_price_overrides.json"), OverrideFile{Version: Version, Overrides: next}); err != nil {
		return fmt.Errorf("save price overrides: %w", err)
	}
	m.overrides = next
	m.rebuildCatalogLocked()
	return nil
}

func overrideKey(provider, model string) string {
	provider = catalogProvider(provider, model)
	return provider + "/" + canonicalModel(provider, model)
}

func validateIdentity(provider, model string) error {
	if provider == "" || model == "" || strings.TrimSpace(provider) != provider || strings.TrimSpace(model) != model || strings.Contains(provider, "/") {
		return fmt.Errorf("provider and model must be non-empty identifiers; provider cannot contain '/'")
	}
	return nil
}

func validateSnapshot(s CatalogSnapshot) error {
	if s.Version != Version {
		return fmt.Errorf("unsupported catalog version %d", s.Version)
	}
	if s.Source == "" || len(s.Entries) == 0 {
		return fmt.Errorf("catalog source and entries are required")
	}
	seen := make(map[string]bool, len(s.Entries))
	for _, e := range s.Entries {
		if err := validateIdentity(e.Provider, e.Model); err != nil {
			return err
		}
		key := e.Provider + "/" + e.Model
		if seen[key] {
			return fmt.Errorf("duplicate catalog entry %s", key)
		}
		seen[key] = true
		if err := e.Price.Validate(); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if e.Disabled || e.Custom {
			return fmt.Errorf("source catalog contains override flags for %s", key)
		}
	}
	return nil
}

func validateOverrides(file OverrideFile) error {
	if file.Version != Version {
		return fmt.Errorf("unsupported override version %d", file.Version)
	}
	for key, ov := range file.Overrides {
		provider, model, ok := strings.Cut(key, "/")
		if !ok {
			return fmt.Errorf("invalid override key %q", key)
		}
		if err := validateIdentity(provider, model); err != nil {
			return err
		}
		if provider != catalogProvider(provider, model) || model != canonicalModel(provider, model) {
			return fmt.Errorf("non-canonical override key %q", key)
		}
		if ov.Price != nil {
			if err := ov.Price.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func readJSON(path string, out any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := readLimited(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func readLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("pricing file exceeds %d bytes", maxResponseBytes)
	}
	return data, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".model-prices-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err = file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func quarantineOverrides(path string) error {
	backup := path + ".bad"
	for attempt := 0; ; attempt++ {
		file, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			backup = fmt.Sprintf("%s.bad.%d.%d", path, time.Now().UnixNano(), attempt)
			continue
		}
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			os.Remove(backup)
			return err
		}
		if err := os.Rename(path, backup); err != nil {
			os.Remove(backup)
			return err
		}
		return nil
	}
}
