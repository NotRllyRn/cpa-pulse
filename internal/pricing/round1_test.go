package pricing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotModifiedRefreshWaitsForNextInterval(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusNotModified)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(status.Load())) }))
	defer server.Close()
	dir := t.TempDir()
	old := time.Now().UTC().Add(-48 * time.Hour)
	snapshot := CatalogSnapshot{Version: Version, Source: "models.dev", FetchedAt: old, CheckedAt: old, ETag: "tag", Entries: EmbeddedEntries()}
	if err := writeJSON(filepath.Join(dir, "model_prices.json"), snapshot); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir, WithURL(server.URL))
	if !m.NeedsRefresh(time.Now(), 24) {
		t.Fatal("old catalog was not due")
	}
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	successful := m.Snapshot()
	if !successful.FetchedAt.Equal(old) || successful.LastSuccessCheckAt.IsZero() || m.NeedsRefresh(time.Now(), 24) {
		t.Fatalf("304 did not refresh check age: %+v", successful)
	}
	if NewManager(dir).NeedsRefresh(time.Now(), 24) {
		t.Fatal("restart lost successful check")
	}
	status.Store(http.StatusServiceUnavailable)
	if err := m.Refresh(context.Background()); err == nil {
		t.Fatal("expected refresh failure")
	}
	if !m.Snapshot().LastSuccessCheckAt.Equal(successful.LastSuccessCheckAt) {
		t.Fatal("failure advanced successful check")
	}
	if m.NeedsRefresh(successful.LastSuccessCheckAt.Add(2*time.Hour), 24) {
		t.Fatal("error backoff replaced successful check age")
	}
	if !m.NeedsRefresh(successful.LastSuccessCheckAt.Add(25*time.Hour), 24) {
		t.Fatal("refresh never became due")
	}
}

func TestEmbeddedSnapshotsStayInMemory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	dir := t.TempDir()
	m := NewManager(dir, WithURL(server.URL))
	if err := m.Refresh(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	path := filepath.Join(dir, "model_prices.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("embedded snapshot persisted: %v", err)
	}
	stale := CatalogSnapshot{Version: Version, Source: "embedded", Entries: []Entry{{Provider: "openai", Model: "stale"}}}
	if err := writeJSON(path, stale); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(dir)
	if _, ok := restarted.Resolve("openai", "stale"); ok {
		t.Fatal("loaded embedded disk snapshot")
	}
	if _, ok := restarted.Resolve("openai", "gpt-5-nano"); !ok {
		t.Fatal("missing bundled fallback")
	}
}

func TestCorruptOverrideQuarantinePreservesBackups(t *testing.T) {
	for _, bad := range []string{"{", `{"version":1,"overrides":{"bad":{"disabled":true}}}`} {
		t.Run(bad, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "model_price_overrides.json")
			for _, item := range []struct{ path, data string }{{path, bad}, {path + ".bad", "earlier backup"}} {
				if err := os.WriteFile(item.path, []byte(item.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			m := NewManager(dir)
			if !strings.Contains(m.Snapshot().LastError, "load price overrides") {
				t.Fatal("corruption warning missing")
			}
			matches, err := filepath.Glob(path + ".bad.*")
			if err != nil || len(matches) != 1 {
				t.Fatalf("backups=%v err=%v", matches, err)
			}
			for name, want := range map[string]string{path + ".bad": "earlier backup", matches[0]: bad} {
				got, err := os.ReadFile(name)
				if err != nil || string(got) != want {
					t.Fatalf("backup=%s data=%s err=%v", name, got, err)
				}
			}
			if err := m.SetOverride("openai", "gpt-5-nano", Override{Disabled: true}); err != nil {
				t.Fatal(err)
			}
			entry, _ := NewManager(dir).Resolve("openai", "gpt-5-nano")
			if !entry.Disabled {
				t.Fatal("replacement override missing")
			}
		})
	}
}

func TestUnreadableOverridesAreNotQuarantinedOrOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model_price_overrides.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir)
	if m.Snapshot().LastError == "" {
		t.Fatal("read failure ignored")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	original := `{"version":1,"overrides":{}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.SetOverride("openai", "gpt-5-nano", Override{Disabled: true}); err == nil {
		t.Fatal("overwrote unread overrides")
	}
	if err := m.DeleteOverride("openai", "gpt-5-nano"); err == nil {
		t.Fatal("deleted unread overrides")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != original {
		t.Fatal("unread overrides changed")
	}
	matches, _ := filepath.Glob(path + ".bad*")
	if len(matches) != 0 {
		t.Fatal("read error quarantined")
	}
	if err := quarantineOverrides(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("failed rename ignored")
	}
	matches, _ = filepath.Glob(filepath.Join(dir, "missing.bad*"))
	if len(matches) != 0 {
		t.Fatal("failed quarantine leaked backup")
	}
}

func TestCatalogCopiesRemainSafeDuringCacheRebuilds(t *testing.T) {
	m := NewManager(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				copy := m.Catalog()
				for k := range copy {
					copy[k].Model = "changed"
				}
				if _, ok := m.Resolve("openai", "gpt-5-nano"); !ok {
					t.Error("caller changed cached lookup")
				}
				_ = m.Rank("codex", []string{"gpt-5-nano"}, "")
			}
		}()
	}
	for i := 0; i < 10; i++ {
		model := fmt.Sprintf("custom-%d", i)
		if err := m.SetOverride("openai", model, Override{Price: &Price{1, 1}}); err != nil {
			t.Fatal(err)
		}
		if err := m.DeleteOverride("openai", model); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestAntigravityNativeCatalogFamilies(t *testing.T) {
	for model, want := range map[string]string{"GPT-5-NANO": "openai", "gpt-5-nano": "openai", "CLAUDE-haiku": "anthropic", "GEMINI-flash": "google"} {
		if got := catalogProvider("antigravity", model); got != want {
			t.Errorf("%s=%s want %s", model, got, want)
		}
	}
	if entry, ok := NewManager(t.TempDir()).Resolve("antigravity", "gpt-5-nano"); !ok || entry.Provider != "openai" {
		t.Fatalf("gpt price=%+v found=%v", entry, ok)
	}
}

func TestFailedOverrideWritesPreserveEffectiveCatalog(t *testing.T) {
	m := NewManager(t.TempDir())
	if err := m.SetOverride("openai", "custom", Override{Price: &Price{1, 2}}); err != nil {
		t.Fatal(err)
	}
	before, ok := m.Resolve("openai", "custom")
	if !ok {
		t.Fatal("missing custom model")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	m.dataDir = blocked
	if err := m.SetOverride("openai", "custom", Override{Disabled: true}); err == nil {
		t.Fatal("failed update ignored")
	}
	if err := m.DeleteOverride("openai", "custom"); err == nil {
		t.Fatal("failed deletion ignored")
	}
	after, ok := m.Resolve("openai", "custom")
	if !ok || after != before {
		t.Fatalf("failed write changed effective catalog: %+v", after)
	}
	if ov := m.Overrides().Overrides["openai/custom"]; ov.Disabled || ov.Price == nil || ov.Price.InputUSDPerMillion != 1 {
		t.Fatalf("failed write changed overrides: %+v", ov)
	}
}

func TestCorruptOverridesUseBadBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model_price_overrides.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir)
	raw, err := os.ReadFile(path + ".bad")
	if err != nil || string(raw) != "{" {
		t.Fatalf("backup=%s err=%v", raw, err)
	}
	info, err := os.Stat(path + ".bad")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions err=%v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("corrupt original remains: %v", err)
	}
	if err := m.SetOverride("openai", "gpt-5-nano", Override{Disabled: true}); err != nil {
		t.Fatal(err)
	}
}
