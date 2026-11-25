// Package plugin implements the CPA plugin RPC methods on top of the engine.
// Package main forwards every native call to Handle.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/NotRllyRn/cpa-pulse/internal/config"
	"github.com/NotRllyRn/cpa-pulse/internal/engine"
	"github.com/NotRllyRn/cpa-pulse/internal/host"
	"github.com/NotRllyRn/cpa-pulse/internal/mcp"
	"github.com/NotRllyRn/cpa-pulse/internal/pricing"
	"github.com/NotRllyRn/cpa-pulse/internal/providers"
	"github.com/NotRllyRn/cpa-pulse/internal/web"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	repository  = "https://github.com/NotRllyRn/cpa-pulse"
	apiBase     = "/v0/management/" + config.PluginID
	pagePath    = "/page"
	stopTimeout = 10 * time.Second
)

// Plugin handles the RPC methods of one loaded plugin instance.
type Plugin struct {
	version     string
	engine      *engine.Engine
	once        sync.Once
	mcp         *mcp.Server
	lifecycleMu sync.Mutex
	stopped     bool
}

// New creates the plugin. dataDir defaults to <plugins.dir>/data/cpa-pulse;
// hostConfigPath is the CPA config file.
func New(h host.Host, version, dataDir, hostConfigPath string) *Plugin {
	p := &Plugin{
		version: version,
		engine: engine.New(engine.Options{
			Host:           h,
			Version:        version,
			DataDir:        dataDir,
			HostConfigPath: hostConfigPath,
		}),
	}
	p.mcp = mcp.New(engine.NewMCPBackend(p.engine), version)
	return p
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func ok(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// ErrorEnvelope encodes a failed RPC result.
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// Handle runs one RPC method and returns the response envelope. A panic is
// turned into an error envelope so that it cannot crash CPA.
func (p *Plugin) Handle(method string, request []byte) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			out = ErrorEnvelope("plugin_panic", fmt.Sprint(r))
		}
	}()
	result, err := p.handle(method, request)
	if err != nil {
		return ErrorEnvelope("plugin_error", err.Error())
	}
	return result
}

// Shutdown stops the engine.
func (p *Plugin) Shutdown() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.stopped = true
	// Close MCP connections before the engine and host callbacks are released.
	p.mcp.Close()
	p.engine.Stop(stopTimeout)
}

func (p *Plugin) status() engine.Status {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	status := p.engine.Status()
	status.MCP = p.mcp.State()
	return status
}

func (p *Plugin) handle(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		cfg, err := config.Parse(req.ConfigYAML)
		p.lifecycleMu.Lock()
		if !p.stopped {
			revision := p.engine.Configure(cfg, err)
			if err == nil {
				p.mcp.Configure(cfg.MCPEnabled, cfg.MCPListen, cfg.MCPKey)
				p.engine.MCPConfigured(revision)
			}
			p.once.Do(p.engine.Start)
		}
		p.lifecycleMu.Unlock()
		return ok(p.registration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		p.Shutdown()
		return ok(struct{}{})
	case pluginabi.MethodManagementRegister:
		return ok(managementRegistration())
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
		return ok(p.handleManagement(req))
	case pluginabi.MethodUsageHandle:
		var record pluginapi.UsageRecord
		if err := json.Unmarshal(request, &record); err == nil {
			p.engine.ObserveUsage(record)
		}
		return ok(struct{}{})
	}
	return ErrorEnvelope("unknown_method", "unknown method: "+method), nil
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]bool    `json:"capabilities"`
}

func field(name string, kind pluginapi.ConfigFieldType, description string) pluginapi.ConfigField {
	return pluginapi.ConfigField{Name: name, Type: kind, Description: description}
}

func (p *Plugin) registration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "CPA Pulse",
			Version:          p.version,
			Author:           "NotRllyRn",
			GitHubRepository: repository,
			ConfigFields: []pluginapi.ConfigField{
				field("config_token", pluginapi.ConfigFieldTypeString, "Configuration acknowledgment token; may be empty"),
				field("bark_url", pluginapi.ConfigFieldTypeString, "Bark push URL up to the device key; empty disables Bark"),
				field("bark_group", pluginapi.ConfigFieldTypeString, "Bark notification group"),
				field("webhook", pluginapi.ConfigFieldTypeObject, "Custom webhook: url, method, headers, body, success_json; empty url disables it"),
				field("mcp_enabled", pluginapi.ConfigFieldTypeBoolean, "Enable the independent MCP HTTP server"),
				field("mcp_listen", pluginapi.ConfigFieldTypeString, "MCP listen address, default 127.0.0.1:8319"),
				field("mcp_key", pluginapi.ConfigFieldTypeString, "Dedicated MCP Bearer key"),
				field("pricing", pluginapi.ConfigFieldTypeObject, "Price refresh interval in hours; 0 disables automatic refresh"),
				field("account_models", pluginapi.ConfigFieldTypeObject, "Manual model by auth ID and quota group key"),
				field("models_api_key", pluginapi.ConfigFieldTypeString, "Dedicated CPA API key for reading /v1/models"),
				field("cpa_base_url", pluginapi.ConfigFieldTypeString, "Address the plugin uses to reach CPA"),
				field("notice_threshold", pluginapi.ConfigFieldTypeNumber, "First alert threshold (percent remaining)"),
				field("low_threshold", pluginapi.ConfigFieldTypeNumber, "Second alert threshold (percent remaining)"),
				field("critical_threshold", pluginapi.ConfigFieldTypeNumber, "Third alert threshold (percent remaining)"),
				field("recovery_notify", pluginapi.ConfigFieldTypeObject, "Recovery notifications, off, all or after_exhausted for five_hour and seven_day"),
				field("reset_reminder", pluginapi.ConfigFieldTypeObject, "Reset reminders, off, all or has_remaining for five_hour and seven_day"),
				field("timezone_offset_hours", pluginapi.ConfigFieldTypeNumber, "UTC offset for displayed times and ignition hours"),
				field("poll_interval_seconds", pluginapi.ConfigFieldTypeInteger, "Active query interval, aligned to the hour"),
				field("passive_skip_seconds", pluginapi.ConfigFieldTypeInteger, "Skip the active query when passive data arrived this many seconds before the slot; 0 never skips"),
				field("passive_skip_max_minutes", pluginapi.ConfigFieldTypeInteger, "Longest run of skipped queries per account, in minutes"),
				field("ignition", pluginapi.ConfigFieldTypeObject, "Ignition switch, daily hours and failure protection"),
				field("providers", pluginapi.ConfigFieldTypeObject, providersDescription()),
				field("codex_reset_updates", pluginapi.ConfigFieldTypeObject, "Forward Did Codex Reset signals"),
				field("history_retention_days", pluginapi.ConfigFieldTypeInteger, "Days of quota history to keep"),
				field("data_dir", pluginapi.ConfigFieldTypeString, "State and history directory, default plugins/data/cpa-pulse"),
			},
		},
		Capabilities: map[string]bool{
			"management_api": true,
			"usage_plugin":   true,
		},
	}
}

type route struct {
	Method      string
	Path        string
	Description string
}

type resource struct {
	Path        string
	Menu        string
	Description string
}

func managementRegistration() map[string]any {
	return map[string]any{
		"routes": []route{
			{Method: http.MethodGet, Path: apiBase + "/status", Description: "cpa-pulse status"},
			{Method: http.MethodGet, Path: apiBase + "/prices", Description: "Local model prices"},
			{Method: http.MethodPost, Path: apiBase + "/prices/refresh", Description: "Refresh model prices"},
			{Method: http.MethodPut, Path: apiBase + "/prices/override", Description: "Set a model price or disable a model"},
			{Method: http.MethodDelete, Path: apiBase + "/prices/override", Description: "Reset a model price override"},
			{Method: http.MethodGet, Path: apiBase + "/history", Description: "cpa-pulse quota history"},
			{Method: http.MethodPost, Path: apiBase + "/refresh", Description: "Query quota now"},
			{Method: http.MethodPost, Path: apiBase + "/ignite", Description: "Ignite one quota window now"},
			{Method: http.MethodPost, Path: apiBase + "/test-notify", Description: "Send a test notification to every channel"},
			{Method: http.MethodPost, Path: apiBase + "/language", Description: "Use English notifications"},
			{Method: http.MethodPost, Path: apiBase + "/check-update", Description: "Check for a newer release"},
		},
		"resources": []resource{
			{Path: pagePath, Menu: "CPA Pulse", Description: "Quota monitoring, ignition and quota chart"},
		},
	}
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		raw = []byte(`{"error":"encode response failed"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
		Body:       raw,
	}
}

func errorResponse(status int, err error) pluginapi.ManagementResponse {
	return jsonResponse(status, map[string]string{"error": err.Error()})
}

func (p *Plugin) handleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimRight(req.Path, "/")
	if path == "/v0/resource/plugins/"+config.PluginID+pagePath {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":  []string{"text/html; charset=utf-8"},
				"Cache-Control": []string{"no-store"},
			},
			Body: web.Page(),
		}
	}
	var body struct {
		AuthIndex string `json:"auth_index"`
		Target    string `json:"target"`
		Language  string `json:"language"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	switch strings.ToUpper(req.Method) + " " + path {
	case "GET " + apiBase + "/prices":
		return jsonResponse(http.StatusOK, p.engine.Prices())
	case "POST " + apiBase + "/prices/refresh":
		p.engine.RefreshPrices()
		return jsonResponse(http.StatusAccepted, map[string]bool{"ok": true, "refreshing": true})
	case "PUT " + apiBase + "/prices/override", "DELETE " + apiBase + "/prices/override":
		var ov struct {
			Provider string   `json:"provider"`
			Model    string   `json:"model"`
			Input    *float64 `json:"input_usd_per_million"`
			Output   *float64 `json:"output_usd_per_million"`
			Disabled bool     `json:"disabled"`
		}
		if len(req.Body) > 0 {
			if err := json.Unmarshal(req.Body, &ov); err != nil {
				return errorResponse(http.StatusBadRequest, errors.New("invalid price override JSON"))
			}
		}
		var err error
		if strings.EqualFold(req.Method, http.MethodDelete) {
			if value := req.Query.Get("provider"); value != "" {
				ov.Provider = value
			}
			if value := req.Query.Get("model"); value != "" {
				ov.Model = value
			}
			err = p.engine.DeletePriceOverride(ov.Provider, ov.Model)
		} else {
			override := pricing.Override{Disabled: ov.Disabled}
			if ov.Input != nil || ov.Output != nil {
				if ov.Input == nil || ov.Output == nil {
					return errorResponse(http.StatusBadRequest, errors.New("input and output prices are both required"))
				}
				override.Price = &pricing.Price{InputUSDPerMillion: *ov.Input, OutputUSDPerMillion: *ov.Output}
			} else if !ov.Disabled {
				return errorResponse(http.StatusBadRequest, errors.New("prices or disabled=true are required"))
			}
			err = p.engine.SetPriceOverride(ov.Provider, ov.Model, override)
		}
		if err != nil {
			return errorResponse(http.StatusBadRequest, err)
		}
		return jsonResponse(http.StatusOK, p.engine.Prices())
	case "GET " + apiBase + "/status":
		return jsonResponse(http.StatusOK, p.status())
	case "GET " + apiBase + "/history":
		resp, err := p.engine.HistoryRange(req.Query.Get("range"), req.Query.Get("window"))
		if err != nil {
			return errorResponse(http.StatusInternalServerError, err)
		}
		return jsonResponse(http.StatusOK, resp)
	case "POST " + apiBase + "/refresh":
		return actionResponse(p.engine.Refresh(strings.TrimSpace(body.AuthIndex)), p)
	case "POST " + apiBase + "/ignite":
		if strings.TrimSpace(body.Target) == "" {
			return errorResponse(http.StatusBadRequest, errors.New("target is required"))
		}
		return actionResponse(p.engine.Ignite(strings.TrimSpace(body.Target)), p)
	case "POST " + apiBase + "/test-notify":
		results, err := p.engine.TestNotify()
		if err != nil {
			return actionResponse(err, p)
		}
		ok := true
		for _, result := range results {
			ok = ok && result.OK
		}
		return jsonResponse(http.StatusOK, map[string]any{"ok": ok, "results": results, "status": p.status()})
	case "POST " + apiBase + "/language":
		return actionResponse(p.engine.SetLanguage("en"), p)
	case "POST " + apiBase + "/check-update":
		return actionResponse(p.engine.CheckRelease(), p)
	}
	return errorResponse(http.StatusNotFound, fmt.Errorf("unknown route %s %s", req.Method, req.Path))
}

func actionResponse(err error, p *Plugin) pluginapi.ManagementResponse {
	if errors.Is(err, engine.ErrNotRunning) {
		return errorResponse(http.StatusServiceUnavailable, err)
	}
	if err != nil {
		return jsonResponse(http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "status": p.status()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "status": p.status()})
}

// providersDescription describes monitoring and ignition settings for each provider ID.
func providersDescription() string {
	ids := providers.IDs()
	english := strings.Join(ids, ", ")
	if n := len(ids); n > 1 {
		english = strings.Join(ids[:n-1], ", ") + " and " + ids[n-1]
	}
	return "Monitoring and ignition settings for " + english
}
