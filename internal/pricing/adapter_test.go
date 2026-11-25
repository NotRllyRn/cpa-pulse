package pricing

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testCatalog = `{"openai":{"models":{
 "chat":{"cost":{"input":1,"output":2},"modalities":{"input":["text","image"],"output":["text"]}},
 "free":{"cost":{"input":0,"output":0},"modalities":{"output":["text"]}},
 "missing":{"modalities":{"output":["text"]}},
 "partial":{"cost":{"input":0},"modalities":{"output":["text"]}},
 "image":{"cost":{"input":1,"output":2},"modalities":{"output":["image"]}},
 "embedding":{"cost":{"input":1,"output":0},"type":"embedding","modalities":{"output":["text"]}},
 "retired":{"status":"retired","cost":{"input":1,"output":2},"modalities":{"output":["text"]}},
 "deprecated":{"status":"deprecated","cost":{"input":1,"output":2},"modalities":{"output":["text"]}},
 "batch":{"batch_only":true,"modalities":{"output":["text"]}},
 "rerank":{"modalities":{"output":["text"]}},
 "whisper-1":{"modalities":{"output":["text"]}},
 "tts-1":{"modalities":{"output":["text"]}},
 "moderation":{"modalities":{"output":["text"]}}
 }},"reseller":{"models":{"chat":{"cost":{"input":0.1,"output":0.2},"modalities":{"output":["text"]}}}}}`

func TestParseModelsDev(t *testing.T) {
	entries, err := ParseModelsDev(strings.NewReader(testCatalog))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.Provider+"/"+e.Model] = e
	}
	for _, id := range []string{"chat", "free", "missing", "partial"} {
		e := byID["openai/"+id]
		if !e.TextGeneration || e.Source != "models.dev" {
			t.Errorf("%s: %+v", id, e)
		}
	}
	for _, id := range []string{"image", "embedding", "retired", "deprecated", "batch", "rerank", "whisper-1", "tts-1", "moderation"} {
		if byID["openai/"+id].TextGeneration {
			t.Errorf("%s allowed text generation", id)
		}
	}
	if !byID["openai/free"].HasPrice || byID["openai/free"].Price != (Price{}) {
		t.Fatal("zero price lost")
	}
	for _, id := range []string{"missing", "partial"} {
		if byID["openai/"+id].HasPrice {
			t.Errorf("%s treated as free", id)
		}
	}
	if byID["reseller/chat"].Provider != "reseller" {
		t.Fatal("full catalog not retained")
	}
	if !byID["openai/retired"].Deprecated || !byID["openai/deprecated"].Deprecated {
		t.Fatal("retired status lost")
	}
}

func TestParseRejectsInvalidCatalogs(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `[]`, `{`, testCatalog + `{}`, `{"openai":{"models":{"bad":{"cost":{"input":-1,"output":1}}}}}`, `{"openai":{"models":{"bad":{"cost":{"output":-1}}}}}`, `{"openai":{"models":{"bad":{"cost":{"input":NaN,"output":1}}}}}`, `{"openai":{"models":{"bad":{"cost":{"input":1e999,"output":1}}}}}`} {
		if _, err := ParseModelsDev(strings.NewReader(body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestPriceValidation(t *testing.T) {
	for _, p := range []Price{{-1, 0}, {0, -1}, {math.NaN(), 0}, {0, math.NaN()}, {math.Inf(1), 0}, {0, math.Inf(-1)}} {
		if err := p.Validate(); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	for _, p := range []Price{{0, 0}, {1, 2}, {math.MaxFloat64, math.MaxFloat64}} {
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRefreshHTTPAndLastGoodSnapshot(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("User-Agent") != "cpa-pulse/1.0" {
			t.Error("missing user agent")
		}
		if calls.Load() > 1 && (r.Header.Get("If-None-Match") != "test-tag" || r.Header.Get("If-Modified-Since") != "Wed, 01 Oct 2025 00:00:00 GMT") {
			t.Error("conditional headers missing")
		}
		w.Header().Set("ETag", "test-tag")
		w.Header().Set("Last-Modified", "Wed, 01 Oct 2025 00:00:00 GMT")
		w.WriteHeader(int(status.Load()))
		if status.Load() == http.StatusOK {
			fmt.Fprint(w, testCatalog)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	m := NewManager(dir, WithURL(server.URL), WithHTTPClient(server.Client()))
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := m.Snapshot()
	if original.Source != "models.dev" || original.FetchedAt.IsZero() {
		t.Fatalf("bad snapshot %+v", original)
	}
	for _, code := range []int{304, 403, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			status.Store(int32(code))
			err := m.Refresh(context.Background())
			current := m.Snapshot()
			if code == 304 && err != nil {
				t.Fatal(err)
			}
			if code != 304 && (err == nil || current.LastError == "") {
				t.Fatal("HTTP error missing")
			}
			if code == 304 && current.LastError != "" {
				t.Fatal(current.LastError)
			}
			if !current.FetchedAt.Equal(original.FetchedAt) || current.CheckedAt.Before(original.CheckedAt) {
				t.Fatal("last good timestamps lost")
			}
			if e, ok := m.Resolve("openai", "chat"); !ok || e.Price.InputUSDPerMillion != 1 {
				t.Fatal("last good price lost")
			}
		})
	}
	cached := NewManager(dir)
	if e, ok := cached.Resolve("openai", "chat"); !ok || e.Price.InputUSDPerMillion != 1 {
		t.Fatal("cache was not persisted")
	}
	status.Store(200)
	if err := m.Refresh(context.Background()); err != nil || m.Snapshot().LastError != "" {
		t.Fatal("error did not clear", err)
	}
}

func TestFailedValidationKeepsSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"openai":{"models":{"bad":{"cost":{"input":-1,"output":0}}}}}`)
	}))
	defer server.Close()
	m := NewManager(t.TempDir(), WithURL(server.URL))
	if err := m.Refresh(context.Background()); err == nil {
		t.Fatal("invalid price accepted")
	}
	if s := m.Snapshot(); s.Source != "embedded" || s.LastError == "" || !s.FetchedAt.IsZero() || s.CheckedAt.IsZero() {
		t.Fatalf("fallback lost: %+v", s)
	}
}

func TestUnexpected304AndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(304) }))
	defer server.Close()
	m := NewManager(t.TempDir(), WithURL(server.URL))
	if err := m.Refresh(context.Background()); err == nil {
		t.Fatal("accepted 304 on embedded fallback")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Refresh(ctx); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestResponseLimit(t *testing.T) {
	if _, err := ParseModelsDev(strings.NewReader(strings.Repeat(" ", maxResponseBytes+1))); err == nil {
		t.Fatal("unbounded catalog accepted")
	}
}

func TestNeedsRefresh(t *testing.T) {
	m := NewManager(t.TempDir())
	now := time.Now().UTC()
	if m.NeedsRefresh(now, 0) || !m.NeedsRefresh(now, 24) {
		t.Fatal("manual or initial refresh wrong")
	}
	m.snapshot.FetchedAt = now.Add(-25 * time.Hour)
	for _, hours := range []int{-1, 24, 169} {
		if !m.NeedsRefresh(now, hours) {
			t.Errorf("%d should use 24h", hours)
		}
	}
	if m.NeedsRefresh(now, 168) {
		t.Fatal("168h interval ignored")
	}
	m.snapshot.CheckedAt = now.Add(-30 * time.Minute)
	m.snapshot.LastError = "offline"
	if m.NeedsRefresh(now, 24) {
		t.Fatal("error backoff ignored")
	}
	if !m.NeedsRefresh(now.Add(time.Hour), 24) {
		t.Fatal("error backoff never expires")
	}
	m.snapshot.LastError = ""
	m.snapshot.FetchedAt = now.Add(-2 * time.Hour)
	if !m.NeedsRefresh(now, 1) || m.NeedsRefresh(now, 24) {
		t.Fatal("refresh interval ignored")
	}
}
