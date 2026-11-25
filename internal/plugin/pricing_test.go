package plugin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/NotRllyRn/cpa-pulse/internal/engine"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func priceRequest(t *testing.T, p *Plugin, method, suffix, body string, query map[string][]string) (pluginapi.ManagementResponse, engine.PricesStatus) {
	t.Helper()
	request, err := json.Marshal(pluginapi.ManagementRequest{Method: method, Path: apiBase + suffix, Body: []byte(body), Query: query})
	if err != nil {
		t.Fatal(err)
	}
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(result(t, p.Handle("management.handle", request)), &response); err != nil {
		t.Fatal(err)
	}
	var prices engine.PricesStatus
	if response.StatusCode == http.StatusOK {
		if err := json.Unmarshal(response.Body, &prices); err != nil {
			t.Fatal(err)
		}
	}
	return response, prices
}

func findPrice(t *testing.T, prices engine.PricesStatus, provider, model string) engine.PriceEntry {
	t.Helper()
	for _, entry := range prices.Entries {
		if entry.Provider == provider && entry.Model == model {
			return entry
		}
	}
	t.Fatalf("price missing: %s/%s", provider, model)
	return engine.PriceEntry{}
}

func TestPriceManagementRoutes(t *testing.T) {
	dir := t.TempDir()
	p := New(nopHost{}, "test", dir, "")
	defer p.Shutdown()
	response, prices := priceRequest(t, p, "GET", "/prices", "", nil)
	if response.StatusCode != 200 || prices.Source != "embedded" || prices.RefreshHours != 24 || prices.AvailableModels == nil || len(prices.Entries) == 0 {
		t.Fatalf("GET %d %+v", response.StatusCode, prices)
	}
	response, prices = priceRequest(t, p, "PUT", "/prices/override", `{"provider":"claude","model":"claude-haiku-5-5","input_usd_per_million":0.01,"output_usd_per_million":0.02,"disabled":false}`, nil)
	row := findPrice(t, prices, "anthropic", "claude-haiku-5-5")
	if response.StatusCode != 200 || !row.Custom || row.Source != "override" || row.InputUSDPerMillion != 0.01 || row.OutputUSDPerMillion != 0.02 || row.EstimatedPulseUSD <= 0 {
		t.Fatalf("PUT %d %+v", response.StatusCode, row)
	}
	restarted := New(nopHost{}, "test", dir, "")
	defer restarted.Shutdown()
	_, persisted := priceRequest(t, restarted, "GET", "/prices", "", nil)
	if row := findPrice(t, persisted, "anthropic", "claude-haiku-5-5"); !row.Custom || row.InputUSDPerMillion != 0.01 {
		t.Fatalf("override not saved: %+v", row)
	}
	response, prices = priceRequest(t, p, "DELETE", "/prices/override", "", map[string][]string{"provider": {"claude"}, "model": {"claude-haiku-5-5"}})
	row = findPrice(t, prices, "anthropic", "claude-haiku-5-5")
	if response.StatusCode != 200 || row.Custom || row.Disabled || row.Source != "embedded" {
		t.Fatalf("DELETE %d %+v", response.StatusCode, row)
	}
	response, prices = priceRequest(t, p, "PUT", "/prices/override", `{"provider":"claude","model":"claude-haiku-5-5","disabled":true}`, nil)
	row = findPrice(t, prices, "anthropic", "claude-haiku-5-5")
	if response.StatusCode != 200 || !row.Disabled || row.Custom {
		t.Fatalf("tombstone %d %+v", response.StatusCode, row)
	}
	response, prices = priceRequest(t, p, "DELETE", "/prices/override", `{"provider":"anthropic","model":"claude-haiku-5-5"}`, nil)
	if row := findPrice(t, prices, "anthropic", "claude-haiku-5-5"); response.StatusCode != 200 || row.Disabled {
		t.Fatalf("DELETE body %d %+v", response.StatusCode, row)
	}
	response, _ = priceRequest(t, p, "POST", "/prices/refresh", "", nil)
	var accepted map[string]bool
	_ = json.Unmarshal(response.Body, &accepted)
	if response.StatusCode != http.StatusAccepted || !accepted["ok"] || !accepted["refreshing"] {
		t.Fatalf("POST %d %s", response.StatusCode, response.Body)
	}
}

func TestPriceOverrideValidation(t *testing.T) {
	p := New(nopHost{}, "test", t.TempDir(), "")
	defer p.Shutdown()
	for _, body := range []string{
		`{`, `{}`, `{"provider":"claude","model":"m"}`,
		`{"provider":"claude","model":"m","input_usd_per_million":1}`,
		`{"provider":"claude","model":"m","input_usd_per_million":-1,"output_usd_per_million":0}`,
		`{"provider":"claude","model":"m","input_usd_per_million":0,"output_usd_per_million":-1}`,
		`{"provider":"claude","model":"m","input_usd_per_million":1e999,"output_usd_per_million":0}`,
		`{"provider":"bad/provider","model":"m","disabled":true}`,
		`{"provider":"claude","model":" ","disabled":true}`,
	} {
		response, _ := priceRequest(t, p, "PUT", "/prices/override", body, nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s: %d %s", body, response.StatusCode, response.Body)
		}
	}
	response, _ := priceRequest(t, p, "DELETE", "/prices/override", "", nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid DELETE: %d", response.StatusCode)
	}
	// Explicit zero prices are valid.
	response, prices := priceRequest(t, p, "PUT", "/prices/override", `{"provider":"anthropic","model":"claude-custom","input_usd_per_million":0,"output_usd_per_million":0}`, nil)
	if row := findPrice(t, prices, "anthropic", "claude-custom"); response.StatusCode != 200 || !row.HasPrice || !row.Custom {
		t.Fatalf("free model: %d %+v", response.StatusCode, row)
	}
}
