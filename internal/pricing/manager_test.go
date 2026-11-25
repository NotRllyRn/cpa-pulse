package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestStartupRecovery(t *testing.T) {
	for _, test := range []struct {
		name, cache, overrides string
		wantError              bool
	}{
		{name: "missing"},
		{name: "corrupt cache", cache: `{`, wantError: true},
		{name: "wrong version", cache: `{"version":2,"source":"models.dev","entries":[]}`, wantError: true},
		{name: "invalid price", cache: `{"version":1,"source":"models.dev","entries":[{"provider":"openai","model":"bad","has_price":true,"price":{"input_usd_per_million":-1}}]}`, wantError: true},
		{name: "corrupt overrides", overrides: `[`, wantError: true},
		{name: "invalid overrides", overrides: `{"version":1,"overrides":{"bad":{"disabled":true}}}`, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if test.cache != "" {
				if err := os.WriteFile(filepath.Join(dir, "model_prices.json"), []byte(test.cache), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if test.overrides != "" {
				if err := os.WriteFile(filepath.Join(dir, "model_price_overrides.json"), []byte(test.overrides), 0600); err != nil {
					t.Fatal(err)
				}
			}
			m := NewManager(dir)
			if (m.Snapshot().LastError != "") != test.wantError {
				t.Fatal(m.Snapshot().LastError)
			}
			if e, ok := m.Resolve("codex", "gpt-5-nano"); !ok || !e.HasPrice || e.Source != "embedded" {
				t.Fatalf("missing fallback: %+v", e)
			}
		})
	}
}

func TestOverridesPersistResetAndSurviveRefresh(t *testing.T) {
	var price atomic.Int32
	price.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"openai":{"models":{"gpt-4o-mini":{"cost":{"input":%d,"output":2},"modalities":{"output":["text"]}}}}}`, price.Load())
	}))
	defer server.Close()
	dir := t.TempDir()
	m := NewManager(dir, WithURL(server.URL))
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := Price{10, 20}
	if err := m.SetOverride("codex", "gpt-4o-mini-2024-07-18", Override{Price: &p}); err != nil {
		t.Fatal(err)
	}
	p.InputUSDPerMillion = 99
	e, _ := m.Resolve("openai", "gpt-4o-mini")
	if !e.Custom || e.Price.InputUSDPerMillion != 10 || e.Source != "override" {
		t.Fatalf("bad override %+v", e)
	}
	p = Price{10, 20}
	if err := m.SetOverride("openai", "gpt-4o-mini", Override{Price: &p, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	price.Store(3)
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(dir)
	e, _ = restarted.Resolve("codex", "gpt-4o-mini")
	if !e.Disabled || e.Price.InputUSDPerMillion != 10 {
		t.Fatalf("override lost on refresh/restart: %+v", e)
	}
	if _, ok := restarted.Cheapest("codex", []string{"gpt-4o-mini"}, ""); ok {
		t.Fatal("tombstone eligible")
	}
	if err := restarted.DeleteOverride("codex", "gpt-4o-mini-2024-07-18"); err != nil {
		t.Fatal(err)
	}
	e, _ = restarted.Resolve("openai", "gpt-4o-mini")
	if e.Custom || e.Disabled || e.Price.InputUSDPerMillion != 3 {
		t.Fatalf("source not restored %+v", e)
	}
	if len(NewManager(dir).Overrides().Overrides) != 0 {
		t.Fatal("delete not persisted")
	}
	for _, name := range []string{"model_prices.json", "model_price_overrides.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]json.RawMessage
		if err = json.Unmarshal(data, &obj); err != nil || string(obj["version"]) != "1" {
			t.Fatal("invalid persisted JSON")
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("permissions %o", info.Mode().Perm())
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".model-prices-") {
			t.Fatal("temporary file leaked")
		}
	}
}

func TestAliasTombstoneSurvivesCatalogIDChange(t *testing.T) {
	var dated atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "claude-sonnet-4-5"
		if dated.Load() {
			id = "claude-sonnet-4-5-20250929"
		}
		fmt.Fprintf(w, `{"anthropic":{"models":{%q:{"cost":{"input":3,"output":15},"modalities":{"output":["text"]}}}}}`, id)
	}))
	defer server.Close()
	m := NewManager(t.TempDir(), WithURL(server.URL))
	if err := m.SetOverride("claude", "claude-sonnet-4-5-20250929", Override{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []bool{false, true, false} {
		dated.Store(value)
		if err := m.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		e, ok := m.Resolve("claude", "claude-sonnet-4-5")
		if !ok || !e.Disabled {
			t.Fatalf("alias tombstone lost: %+v", e)
		}
	}
}

func TestUnknownOverridesAndCopies(t *testing.T) {
	m := NewManager(t.TempDir())
	p := Price{}
	if err := m.SetOverride("openai", "private-model", Override{Price: &p}); err != nil {
		t.Fatal(err)
	}
	e, ok := m.Resolve("codex", "private-model")
	if !ok || !e.HasPrice || !e.TextGeneration || !e.Custom {
		t.Fatalf("custom model missing %+v", e)
	}
	snap := m.Snapshot()
	snap.Entries[0].Model = "changed"
	catalog := m.Catalog()
	catalog[0].Model = "changed"
	ov := m.Overrides()
	ov.Overrides["openai/private-model"].Price.InputUSDPerMillion = 123
	if e, _ = m.Resolve("codex", "private-model"); e.Price.InputUSDPerMillion != 0 {
		t.Fatal("override pointer leaked")
	}
	if m.Snapshot().Entries[0].Model == "changed" || m.Catalog()[0].Model == "changed" {
		t.Fatal("entry slice leaked")
	}
	if err := m.DeleteOverride("openai", "private-model"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Resolve("openai", "private-model"); ok {
		t.Fatal("unknown model retained after delete")
	}
	if err := m.SetOverride("openai", "private-model", Override{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Cheapest("openai", []string{"private-model"}, ""); ok {
		t.Fatal("unpriced tombstone eligible")
	}
}

func TestOverrideValidationAndWriteFailure(t *testing.T) {
	m := NewManager(t.TempDir())
	negative := Price{-1, 0}
	for _, test := range []struct {
		provider, model string
		ov              Override
	}{
		{"", "m", Override{}}, {"p", "", Override{}}, {"p/q", "m", Override{}}, {"p", "m", Override{Price: &negative}},
	} {
		if err := m.SetOverride(test.provider, test.model, test.ov); err == nil {
			t.Fatalf("accepted %+v", test)
		}
	}
	if err := m.DeleteOverride("", "m"); err == nil {
		t.Fatal("invalid delete accepted")
	}
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	blocked := NewManager(path)
	if err := blocked.SetOverride("openai", "gpt-4o-mini", Override{Disabled: true}); err == nil {
		t.Fatal("write failure ignored")
	}
	if e, _ := blocked.Resolve("openai", "gpt-4o-mini"); e.Disabled {
		t.Fatal("failed override mutated memory")
	}
}

type joinedContext struct {
	context.Context
	ready chan struct{}
	once  sync.Once
}

func (c *joinedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Done()
}

func TestConcurrentSingleFlightRefresh(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		fmt.Fprint(w, testCatalog)
	}))
	defer server.Close()
	m := NewManager(t.TempDir(), WithURL(server.URL))
	errors := make(chan error, 33)
	go func() { errors <- m.Refresh(context.Background()) }()
	<-entered
	for i := 0; i < 32; i++ {
		ctx := &joinedContext{Context: context.Background(), ready: make(chan struct{})}
		go func() { errors <- m.Refresh(ctx) }()
		<-ctx.ready
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := Price{float64(i), 1}
			if err := m.SetOverride("openai", fmt.Sprintf("custom-%d", i), Override{Price: &p}); err != nil {
				t.Error(err)
			}
			_ = m.Catalog()
			_ = m.Snapshot()
			_ = m.Overrides()
			_ = m.Rank("openai", []string{"chat", "custom-1"}, "")
		}(i)
	}
	close(release)
	for i := 0; i < 33; i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("got %d fetches", calls.Load())
	}
	if len(m.Overrides().Overrides) != 8 {
		t.Fatal("concurrent overrides lost")
	}
}

func TestWaitingRefreshCanCancel(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; fmt.Fprint(w, testCatalog) }))
	defer server.Close()
	m := NewManager(t.TempDir(), WithURL(server.URL))
	result := make(chan error, 1)
	go func() { result <- m.Refresh(context.Background()) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	joined := &joinedContext{Context: ctx, ready: make(chan struct{})}
	waiter := make(chan error, 1)
	go func() { waiter <- m.Refresh(joined) }()
	<-joined.ready
	cancel()
	if err := <-waiter; err != context.Canceled {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
