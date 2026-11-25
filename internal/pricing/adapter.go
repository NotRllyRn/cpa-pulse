package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const maxResponseBytes = 25 << 20

type wireModel struct {
	Cost *struct {
		Input  *float64 `json:"input"`
		Output *float64 `json:"output"`
	} `json:"cost"`
	Modalities struct {
		Output []string `json:"output"`
	} `json:"modalities"`
	Status    string `json:"status"`
	Type      string `json:"type"`
	Mode      string `json:"mode"`
	Family    string `json:"family"`
	BatchOnly bool   `json:"batch_only"`
}

// ParseModelsDev reads a complete provider-scoped models.dev v1 catalog.
func ParseModelsDev(r io.Reader) ([]Entry, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("catalog exceeds %d bytes", maxResponseBytes)
	}
	var providers map[string]struct {
		Models map[string]wireModel `json:"models"`
	}
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	var entries []Entry
	for provider, p := range providers {
		for model, m := range p.Models {
			if err := validateIdentity(provider, model); err != nil {
				return nil, err
			}
			e := Entry{Provider: provider, Model: model, Source: "models.dev"}
			status := strings.ToLower(m.Status)
			e.Deprecated = status == "deprecated" || status == "retired"
			for _, modality := range m.Modalities.Output {
				if modality == "text" {
					e.TextGeneration = true
				}
			}
			if e.Deprecated || nonChat(model, m) {
				e.TextGeneration = false
			}
			if m.Cost != nil {
				var p Price
				if m.Cost.Input != nil {
					p.InputUSDPerMillion = *m.Cost.Input
				}
				if m.Cost.Output != nil {
					p.OutputUSDPerMillion = *m.Cost.Output
				}
				if err := p.Validate(); err != nil {
					return nil, fmt.Errorf("%s/%s: %w", provider, model, err)
				}
				if m.Cost.Input != nil && m.Cost.Output != nil {
					e.Price = p
					e.HasPrice = true
				}
			}
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("catalog contains no models")
	}
	sortEntries(entries)
	return entries, nil
}

func nonChat(id string, m wireModel) bool {
	if m.BatchOnly {
		return true
	}
	// Some embedding and transcription models advertise text output.
	for _, value := range []string{m.Type, m.Mode, m.Family, id} {
		value = strings.ToLower(value)
		for _, marker := range []string{"embedding", "embed-", "rerank", "moderation", "transcri", "whisper", "tts", "speech-to-text", "text-to-speech", "batch-only"} {
			if strings.Contains(value, marker) {
				return true
			}
		}
	}
	switch strings.ToLower(m.Type) {
	case "image", "audio", "video", "stt", "batch":
		return true
	}
	switch strings.ToLower(m.Mode) {
	case "image", "audio", "video", "stt", "batch":
		return true
	}
	return false
}

func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Provider != entries[j].Provider {
			return entries[i].Provider < entries[j].Provider
		}
		return entries[i].Model < entries[j].Model
	})
}

func (m *Manager) fetch(ctx context.Context, previous CatalogSnapshot) (CatalogSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.url, nil)
	if err != nil {
		return previous, err
	}
	req.Header.Set("User-Agent", "cpa-pulse/1.0")
	if previous.ETag != "" {
		req.Header.Set("If-None-Match", previous.ETag)
	}
	if previous.LastModified != "" {
		req.Header.Set("If-Modified-Since", previous.LastModified)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return previous, fmt.Errorf("fetch model prices: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		if previous.Source != "models.dev" || (previous.ETag == "" && previous.LastModified == "") {
			return previous, fmt.Errorf("unexpected HTTP 304 without a cached catalog validator")
		}
		previous.CheckedAt = time.Now().UTC()
		previous.LastSuccessCheckAt = previous.CheckedAt
		previous.LastError = ""
		if v := resp.Header.Get("ETag"); v != "" {
			previous.ETag = v
		}
		if v := resp.Header.Get("Last-Modified"); v != "" {
			previous.LastModified = v
		}
		return previous, nil
	}
	if resp.StatusCode != http.StatusOK {
		return previous, fmt.Errorf("fetch model prices: HTTP %d", resp.StatusCode)
	}
	entries, err := ParseModelsDev(resp.Body)
	if err != nil {
		return previous, err
	}
	now := time.Now().UTC()
	return CatalogSnapshot{Version: Version, Source: "models.dev", FetchedAt: now, CheckedAt: now, LastSuccessCheckAt: now, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"), Entries: entries}, nil
}
