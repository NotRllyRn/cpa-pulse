// Package pricing stores model prices and ranks small text-generation requests.
package pricing

import (
	"fmt"
	"math"
	"time"
)

const (
	Version             = 1
	DefaultRefreshHours = 24
	ModelsDevURL        = "https://models.dev/api.json"
)

type Price struct {
	InputUSDPerMillion  float64 `json:"input_usd_per_million"`
	OutputUSDPerMillion float64 `json:"output_usd_per_million"`
}

// EstimatedPulseUSD estimates a request with 32 input and 2 output tokens.
func (p Price) EstimatedPulseUSD() float64 {
	// Divide first so finite prices remain finite.
	return (32.0/1_000_000)*p.InputUSDPerMillion + (2.0/1_000_000)*p.OutputUSDPerMillion
}

func (p Price) Validate() error {
	if math.IsNaN(p.InputUSDPerMillion) || math.IsInf(p.InputUSDPerMillion, 0) || p.InputUSDPerMillion < 0 || math.IsNaN(p.OutputUSDPerMillion) || math.IsInf(p.OutputUSDPerMillion, 0) || p.OutputUSDPerMillion < 0 {
		return fmt.Errorf("input and output prices must be finite and non-negative")
	}
	return nil
}

type Entry struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Price          Price  `json:"price"`
	HasPrice       bool   `json:"has_price"`
	TextGeneration bool   `json:"text_generation"`
	Deprecated     bool   `json:"deprecated"`
	Source         string `json:"source"`
	Disabled       bool   `json:"disabled"`
	Custom         bool   `json:"custom"`
}

type Override struct {
	Price    *Price `json:"price,omitempty"`
	Disabled bool   `json:"disabled"`
}

type CatalogSnapshot struct {
	Version            int       `json:"version"`
	Source             string    `json:"source"`
	FetchedAt          time.Time `json:"fetched_at"`
	CheckedAt          time.Time `json:"checked_at"`
	LastSuccessCheckAt time.Time `json:"last_success_check_at"`
	ETag               string    `json:"etag,omitempty"`
	LastModified       string    `json:"last_modified,omitempty"`
	LastError          string    `json:"last_error,omitempty"`
	Entries            []Entry   `json:"entries"`
}

type OverrideFile struct {
	Version   int                 `json:"version"`
	Overrides map[string]Override `json:"overrides"`
}

type RankedCandidate struct {
	ModelID           string  `json:"model_id"`
	Provider          string  `json:"provider"`
	CatalogProvider   string  `json:"catalog_provider"`
	CatalogModel      string  `json:"catalog_model"`
	Price             Price   `json:"price"`
	EstimatedPulseUSD float64 `json:"estimated_pulse_usd"`
	Source            string  `json:"source"`
	Confirmed         bool    `json:"confirmed"`
	Eligible          bool    `json:"eligible"`
	Reason            string  `json:"reason,omitempty"`
}
