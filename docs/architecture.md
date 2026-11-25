# Architecture

[Usage](usage.md) · [Configuration](configuration.md) · [Development](development.md)

cpa-pulse is a Go native plugin built with `-buildmode=c-shared`. CPA loads the library and calls `cliproxy_plugin_init`. Subsequent calls use JSON RPC envelopes in both directions. The Go module is `github.com/NotRllyRn/cpa-pulse`; the runtime plugin ID is `cpa-pulse` and configuration lives under `plugins.configs.cpa-pulse`.

## Modules

| Path | Responsibility |
| --- | --- |
| `main.go` | cgo entry point, C ABI exports, RPC forwarding and host callbacks |
| `internal/plugin` | Registration, reconfiguration, quiescence, management API and `usage.handle` |
| `internal/engine` | Polling, passive readings, ignition, notifications, history and status; `mcp.go` supplies model-readable DTOs and forecasts |
| `internal/mcp` | Independent HTTP listener, Streamable HTTP protocol, Bearer authentication and Origin checks |
| `internal/host` | Host callback wrappers, model stream lifecycle and HTTP cancellation |
| `internal/config` | Configuration parsing, defaults, limits, host enablement and default data directory |
| `internal/providers` | Provider requests, parsing, candidate models, plans and registry |
| `internal/quota` | Quota windows, request types, window classification and time parsing |
| `internal/egress` | Network exit selection |
| `internal/models` | CPA model list and candidate ordering |
| `internal/pricing` | models.dev v1 adapter, local catalog/overrides, offline fallback and micro-pulse cost ranking |
| `internal/ignite` | Scheduling, sliding-reset detection, error classification and failure protection |
| `internal/failure` | Credential, suspension and rate-limit diagnoses and short error summaries |
| `internal/notify` | Bark, webhook, quota alerts and Did Codex Reset forwarding |
| `internal/release` | Latest GitHub release and numeric version comparison |
| `internal/store` | State, history and instance lock |
| `internal/web` | Management page, edited as separate HTML, CSS and JS and embedded as one document |

## CPA interface

`plugin.register` returns `schema_version: 6` and declares `management_api` and `usage_plugin`. Usage records include upstream response headers. Management data requests require the CPA management key.

| Method and path | Purpose |
| --- | --- |
| `GET /v0/management/cpa-pulse/status` | Account, quota, schedule and redacted configuration snapshots; `providers` lists IDs, titles, ignition capability, model hints and selectable groups; `group_order` sets quota-group order; windows include `kind` for `5h`, `7d` and `30d`; targets expose per-window schedules |
| `GET /v0/management/cpa-pulse/history` | History and ignition events; accepts `range` and `window`, described below |
| `POST /v0/management/cpa-pulse/refresh` | Active refresh; `{"auth_index":"..."}` selects one account |
| `POST /v0/management/cpa-pulse/ignite` | Manual ignition; `{"target":"<quota-group key>"}` |
| `POST /v0/management/cpa-pulse/test-notify` | Test all configured channels; each `results` entry has `channel`, `ok` and `error`; webhook entries add `response` (first 200 characters) and `unchecked` when only HTTP status was checked |
| `POST /v0/management/cpa-pulse/language` | Compatibility endpoint for notification language; notifications use English |
| `POST /v0/management/cpa-pulse/check-update` | Check the latest release and return status |

The page resource is `GET /v0/resource/plugins/cpa-pulse/page`, with the menu title `cpa-pulse`. The resource is public, but its data requests carry the management key. The page reads `cli-proxy-auth` from Management Center local storage: `enc::v2::` uses reversible XOR based on `location.host`, `enc::v1::` also uses User-Agent, and unprefixed values are plain JSON. If no key is found, the page asks for one and keeps it in session storage under `cpa-pulse-management-key`.

History accepts `range=1h|3h|6h|12h|24h|26h|4d|8d|15d|1mo|36d|31d|61d|90d|151d`, default `24h` (`31d` when `window=30d` and range is omitted). `window=5h|7d|30d` filters series; an omitted or unrecognized value returns all windows. Ignition events are not window-filtered. The chart's one-day option also uses `24h`. A month starts on the same date in the previous month in the plugin time zone, clamped to that month's last day. Each series can include its last sample before the range, up to one day earlier (`historyLead`), so the page can clip the line at the left edge. A series without in-range samples is omitted. See [Storage](#storage) for downsampling and reset records.

For example, `GET /v0/management/cpa-pulse/history?window=30d&range=31d` returns 30-day history. Its chart range choices are `24h`, `15d`, `31d`, `61d`, `90d` and `151d`; retention still limits available samples.

### Model prices API

All paths below use `/v0/management/cpa-pulse` and require the CPA management key.

| Method and path | Purpose |
| --- | --- |
| `GET /prices` | Local effective catalog, source, fetch/check times, refresh state/error, CPA availability, eligibility and estimated pulse cost; performs no network fetch |
| `POST /prices/refresh` | Start a shared background fetch; returns HTTP 202 with `refreshing: true`; poll `GET /prices` for completion |
| `PUT /prices/override` | Set custom prices or a removal tombstone |
| `DELETE /prices/override` | Delete a local override and restore the source catalog entry |

PUT takes `provider`, `model`, optional `disabled`, and both `input_usd_per_million` and `output_usd_per_million`. Prices must be finite and nonnegative; zero is valid. `disabled: true` may omit prices and creates a tombstone that survives refresh. DELETE takes `provider` and `model` in query parameters or JSON; query parameters take precedence. PUT and DELETE return the effective prices snapshot. Resetting an override-only model removes its entry. Provider aliases and known model aliases share canonical overrides.

### Configuration saves

Settings use CPA's `PATCH /v0/management/plugins/cpa-pulse/config`. Switches save on change, text and numbers on blur or Enter. Only affected top-level keys are patched. CPA merges only the top level, so nested objects such as `ignition`, `providers` and notification modes are submitted whole using the last saved object as the base. Saves run serially. The three thresholds are validated and saved together. Invalid numeric ranges or steps prevent saving. URL trailing slashes are removed; empty CPA and MCP addresses are normalized to backend defaults.

The model API-key field rejects the management key. Other nonempty values are checked with `GET /v1/models`; 401 or 403 prevents saving, while other responses or transport failures permit saving. Browser autofill detected while the field lacks focus is ignored and restored to the saved value. The field uses `autocomplete="one-time-code"` and password-manager skip attributes to avoid unwanted credential insertion. Rejected edits retain their drafts and errors.

Webhook headers are edited one `Name: value` per line, and success conditions as a JSON object. Unparseable values cannot save. Management status exposes only whether webhook URL, headers and body are set; the page reads their original values from CPA's plugin configuration endpoint.

Every save carries a fresh `config_token`: eight cryptographically random bytes rendered as 16 hexadecimal characters. It is an acknowledgment token, not a secret. `status.config` describes the latest accepted configuration; top-level `config_applied_token` acknowledges the same configuration after the engine applies it and `mcp.Configure` completes. MCP bind failure still completes configuration application and appears in `mcp.error`; availability requires `mcp.running`. Invalid configuration retains the preceding token. Instance-local receive sequence numbers and a lifecycle lock prevent snapshots from mixing configurations. Restarting confirms the token supplied by CPA without a persisted counter.

After saving, `whenApplied` polls status one second after each completed request, with a four-second request timeout and an independent 15-second overall deadline. The save is confirmed by its token or a later token issued by this page. A deadline yields an unconfirmed result even if a response arrives later. Secret placeholders and `mcp_key_set` cannot confirm key replacement. Send test waits for queued saves and the same acknowledgment; it sends nothing if confirmation times out.

`captureIntent` stores field values and edit generations; queued `buildPatch` uses that snapshot. `refillSaved` clears only drafts actually submitted, whose generations still match and whose independent failure state is clear. Radio groups share an edit generation and refill as a unit. `claimState` assigns per-item ownership; a later input revokes old ownership so late responses cannot overwrite newer results. `settingsDirty` and `accountDirty` protect drafts. Failures persist until that item saves successfully, including through unrelated successful saves.

### Accounts and page state

Names use CPA's `PATCH /v0/management/auth-files/fields` to replace the first line of a credential's `note`, preserving later lines. CPA trims notes, so clearing a multiline name requires confirmation and deletes the whole note. A subsequent account refresh updates the displayed name; refresh failure does not undo a successful rename. Positive finite weights are stored by credential filename in the whole `account_weights` object; clearing a weight restores the plan default.

Account rows reuse DOM by credential filename to preserve focus and drafts through refresh and reordering. Save results are keyed independently from DOM bindings; rebuilding a row restores its drafts, failures and pending count. Drag handles use Pointer Events, a fixed-position clone, midpoint hit testing and 180 ms FLIP movement. Dropping saves `account_order`; Esc or `pointercancel` restores the original order. Rendering waits until dragging finishes, and reduced-motion preferences suppress animation.

The page stores its view in `cpa-pulse-view`, blur state in `cpa-pulse-mask-accounts`, optional MCP public URL in `cpa-pulse-mcp-url`, chart filters in `cpa-pulse-chart-filters` and expanded groups in `cpa-pulse-chart-expanded`. Filters keep a range per window; expanded group IDs use `provider|source_label` and one shared set for all window kinds. Updates reread the stored list before changing one group to preserve other tabs' edits. Disabling a remember switch removes its stored choices; enabling merges the current selection. Restoration happens after configuration loads and before the first history request.

Blur state is applied to the root before first render. HTML account identifiers use `.account-sensitive` and CSS blur; SVG text uses a filter. Sensitive values include current identifiers, retained event/history labels and internal group IDs of orphan history. Orphan rows are blurred across overview, detail, table and screenshots and use account numbers for accessibility labels. Error summaries and raw errors are blurred whole. Legacy warning/error details without structured parameters are also blurred whole because account names may no longer be known. Sensitive hover text is omitted while blurred. Confirmation dialogs retain original text; raw DOM and API data remain available.

Card headers contain the service label, nondefault weight and email. The bottom `.account-head.cell-note` row places the plan left and Refresh then camera right; `.no-snapshot` hides actions from exports. `snapshotPng` clones its target, removes actions, embeds computed styles and uses SVG `foreignObject` and canvas to produce a device-pixel-ratio PNG for `ClipboardItem`. Detail captures use `#chart-detail-wrap`. `.snapshot-frame` supplies shared measurement/export styling. No external assets are loaded during capture; animation, transitions and hover state are omitted. Secure context, image-clipboard support and browser permission are required.

`saveHost` locates one `.save-state` per section heading or Advanced legend. `saveEntries` tracks item results and ownership. The latest initiated save supplies normal progress; unresolved failures take priority and show their count. Failed controls carry `aria-invalid`. Desktop headings truncate field and reason separately; at 640 px or less those parts are hidden while the failure count stays visible. Test results use `#test-results`; copy feedback uses a toast.

MCP controls show `status.mcp`. Enabling without a key generates 32 random bytes as base64url with `llmcp_` prefix in the same save. The optional public URL belongs only to the browser. Copies read its current stored value to avoid using an old tab's stale URL; `storage` events sync the input. Validation trims the same whitespace as Go `strings.TrimSpace`, preserves U+FEFF, assumes HTTPS without a scheme and permits printable ASCII host/path syntax without spaces, `?`, `#`, `\` or bracketed IPv6. Ports cannot exceed 65535. The fixtures are in `internal/web/testdata/public_urls.json`. Valid paths have trailing `/` and `/mcp` normalized before adding `/mcp`; missing or invalid URLs use the listen address, replacing wildcard hosts with the page hostname. Shell examples use POSIX single-quote escaping; JSON uses `JSON.stringify`.

The logo loads from this repository's `assets/logo.png` on GitHub. If it fails or takes more than five seconds, the page retries through `ghfast.top`; if that also fails, it hides the logo.

The page uses English text. Existing history, account notes, upstream errors and saved notification titles remain verbatim. Structured event parameters generate current event text; unstructured old events retain their stored text.

### Events and theme

Notification events attach to their triggering event using `NOTIFY_TRIGGERS`: recovery/reset, circuit/ignition pause, cooldown/stale cooldown, plan/plan change, credential variants/credential invalid, credential recovery/credential restored, and reset signal/reset signal. They must match group, or label when no group exists; reset signals require no group. The nearest trigger is selected within 70 minutes after the event, or within five minutes either way for reset signals, whose notification precedes recording. Recovery `windows` matches individual window IDs; old recovery records without it can attach to same-group resets within 60 seconds. Unmatched notifications and quota/reminder notifications remain separate rows. Legacy kinds are inferred from titles.

Five-hour reset events and their notifications are hidden by default, controlled by `cpa-pulse-events-5h`; exhaustion events always show. Errors use `params.summary` or a parsed first-line summary, truncated to 160 characters, with full details on hover. Manual failures omit retry text. Credential invalid events show authentication, suspension or rate-limit badges. A same-group ignition pause within 60 seconds (`PAUSE_PAIR`) is merged into the nearest credential event. Details include suspension/authentication state or pause/limit end time; hover adds status, code, summary, appeal URL and merged error. Appeal links must use HTTP or HTTPS; sensitive hover details are hidden while blurred.

Themes reuse the Management Center CSS variables from `src/styles/themes.scss` for light, pure-white and dark. `quotaLevel` shares five levels across bars and charts: full near 100%, empty near 0% (`LEVEL_EPSILON=1e-6`), then high/medium/low split at the first two alert thresholds. Classification uses raw values before rounding. The page follows the parent root's `data-theme`, or `cli-proxy-theme` when standalone. Segmented selectors share measured thumb positioning via `placeThumb` and `ResizeObserver`; only deliberate user selection animates, and reduced-motion rules suppress it.

Embedded layout reserves space for the Management Center toolbar: 178 × 52 px with 24 px offsets, or 162 × 48 px with 12 px offsets at widths up to 768 px, plus its 48 × 48 px mobile menu. `embedded` detection needs no parent access. Desktop layouts wider than 1584 px need no reservation. These dimensions follow `src/styles/layout.scss` and must stay aligned if the host changes.

### Chart calculations

The user-facing chart behavior and available ranges are in [Usage](usage.md#management-page). Its calculations run in `page.js`:

- The x-axis ends now for 1/3 hours and 1/4 days. Longer ranges end at the selected row's next reset, taking the earliest future reset for totals. Missing, past or out-of-range resets fall back to now. The axis stays fixed as time passes and advances on reset. Overview and detail share the axis; future and missing intervals are hatched.
- A gap exceeds 3.5 polling/bucket intervals. With passive skipping enabled, the threshold is at least `passive_skip_max_minutes` plus 1.5 poll intervals so planned gaps are not marked missing.
- `pooled` averages remaining percentages by account weight: explicit weight, plan default, then 1. The detail band shows the account minimum/maximum. A missing account is estimated by `gapValue`: interpolate a declining same-cycle gap; keep the last value until a known reset and interpolate from 100% afterward; preserve the old value when a reset jump lacks a reset time. After the last sample, hold until reset, then assume 100%. An account with no samples contributes nothing. Totals only exist where at least one account has recent data. Estimated points carry a sixth-value marker and draw dashed; an estimated latest point has no projection.
- Current windows and retained history form the row union, classified by kind. Current windows lacking history get empty rows without invented samples. Retired windows remain separate, excluded from totals and lowest-first ordering, without references or projections. A window with its own label remains separate from its group's main quota.
- Default order follows `account_order`, then provider registration and account suffix. Missing entries are ignored and the first duplicate wins. Groups follow `group_order`: Claude, GPT, Gemini, Kimi, Grok, Fable, Claude / GPT. A total takes its first current account's position.
- History is cached by window/range and rendered immediately on revisits while refreshing. Only the latest requested selection may render; stale responses still populate cache. Duplicate requests are reused except after refresh/ignition. Errors without matching cached data replace the old chart. The alternate window is prefetched after the first chart. Status is polled every 30 seconds; newer active samples or changed passive values trigger history refresh. Rendering waits while the pointer is over the chart.
- A rise of at least five percentage points (`RESET_JUMP`) breaks the curve into cycles; rises separated by one sample count as one reset. Smaller rises in totals do not break the total curve. Overview colors use the cumulative cycle minimum to avoid threshold flicker from rounding.
- Detail curves fit each cycle to a nonincreasing sequence by pooled adjacent violators, then use Fritsch-Carlson monotone cubic interpolation. Tooltips retain raw samples. Steps of at most two points connect as a decline; larger declines follow a flat interval before bending down.

`rowPace` uses the history API's `resets`, with fixed length `L` of five hours, seven days or thirty days and value `100 × (reset − time) / L`. Each reset record applies from the later of its observation or cycle start until the next record or its reset. Unknown reset `0` draws no reference. An early reset truncates the old reference. Current state adds a record at history response `to` when the reset differs by more than 60 seconds or becomes unknown. Unknown transitions exclude that boundary from the old segment, so headers and totals show no reference at that instant. Absent current windows pass `null` and add no record. Totals require a reference for every current account.

`paceExtensions` extends a changed reference backward to the last five-point rise after `reset − L − 60 seconds`; unchanged resets within `RESET_DRIFT=60` do not extend. Extensions use a dash-dot line and affect only the plotted weighted reference and hover explanation, not current headers or MCP. Detail headings show remaining, reference, difference and reset countdown; green/red indicates above/below pace. For axes ending now, end labels are spaced by `LABEL_H`. `placeDetailHead` places headings in the left margin when the plot is at least 640 px wide with `labelW ≥ 190`, reserving `SIDE_GAP=46` for ticks; otherwise headings stay above.

Five-hour projection uses `currentRate`: cycle cumulative minima, bottom-up least-squares segment merging within `QUANTUM=2` percentage points, then the last segment's slope (`recentSlope`). Passive samples extend to now only after `HOLD_AFTER=2 minutes`; active samples after 1.5 sampling intervals. Before then, the curve and projection begin/end at the sample time. Seven-day and thirty-day projections use `cycleRate`: consumed quota divided by elapsed time since the current reset minus the window length, including idle time. Unknown/expired resets, future starts or samples before cycle start suppress it. Totals weight valid account rates. A nondeclining rate draws horizontally. Projections stop at exhaustion or reset and label the endpoint.

The seven-day cycle average reduces sensitivity to concentrated evening use. Upstream's replay over two seven-day cycles measured mean remaining-quota error of 9.9 points for the cycle average, versus 20.0 for the latest slope and 13.5/12.3 for 24/48-hour lookbacks. Across 69 five-hour cycles, forecasts did not beat assuming no further use, so that projection represents current pace only. No replay cycle exhausted quota; exhaustion-time accuracy is unverified.

Hover snaps to the nearest detail-series sample and uses preceding readings from other series. If even the nearest sample lies beyond the gap threshold, it follows pointer time. Future hover shows projected and reference values only in the detail chart.

### Host callbacks

| Callback | Use |
| --- | --- |
| `host.auth.list` | Credential indices, runtime IDs, emails, notes, disabled state and CPA cooldown (`unavailable`, `next_retry_after`) |
| `host.auth.get` | Credential JSON, token/fingerprint, account ID, provider metadata and `proxy_url` |
| `host.model.execute_stream`, `stream_read`, `stream_close` | Account-pinned ignition; close cancels the upstream request |
| `host.http.operation_open`, `host.http.do`, `host.http.cancel` | Cancellable quota requests without account proxy and Hub manifest requests |
| `host.log` | CPA log records |

Callbacks belong to the plugin instance and remain usable outside any specific incoming RPC. Each poll reloads credentials and computes account suffixes from the first note line (up to 20 characters), adding email suffixes to disambiguate. Without a name, an email suffix is needed only for multiple accounts of one provider. Alert state uses stable group keys, so renaming does not resend alerts.

## MCP

Connection, client configuration and proxy examples are in [MCP](mcp.md). The endpoint uses its own `net.Listen` and `http.Server`, independent of CPA management routing and authentication.

`plugin.Plugin` owns the server and lifecycle mutex. Valid registration/reconfiguration applies enablement, address and key. Changing the address closes the old listener before binding. Rotating only a nonempty key updates it in place and cancels requests accepted with the old key, preserving keep-alive connections. New requests authenticate with the new key after `Configure` returns. Empty keys prevent startup; bind failures are recorded and retried every two seconds for hot reload. Quiesce and shutdown close the listener, cancel connections and wait for tools before stopping the engine. Stopped instances cannot restart. Management `mcp` contains `enabled`, `listen`, `running`, `error` and `path`; `config.mcp_key_set` exposes only presence, while CPA configuration GET/PATCH retains the actual key.

### Protocol and authentication

The stateless endpoint implements `initialize`, `notifications/initialized`, `ping`, `tools/list` and `tools/call`. It negotiates `2025-03-26` and `2025-06-18`, recommending the latter for unsupported versions. Subsequent `MCP-Protocol-Version`, when supplied, must be supported. POST returns JSON, notifications return empty 202 and GET returns 405. The older protocol allows batches of at most 50 items; the newer rejects batches. Request bodies are limited to 1 MiB with read/write timeouts. Four tool calls may execute concurrently, including batch calls; excess calls immediately receive JSON-RPC `-32000` without queuing. Query timeouts are 10 seconds and refresh is 180 seconds. History and forecast loops honor cancellation.

Bearer keys are SHA-256 hashed on both sides before constant-time comparison. If present, Origin must be one HTTP/HTTPS localhost or loopback-IP origin; arbitrary hostname resolution and multiple Origins are rejected. Proxies must preserve external Origins rather than rewriting them to local ones. Authentication/transport errors use HTTP statuses; protocol errors use JSON-RPC codes; tool failures use `isError`.

### Tool data

`engine.MCPBackend` reads locked engine snapshots and emits dedicated DTOs. It omits full emails, credential filenames, auth indices, internal group keys, data paths, configuration and notification secrets. Upstream failures become fixed query/authentication/rate-limit states. Events omit raw errors, summaries, appeal URLs and arbitrary text. Account labeling, selection and readiness states are detailed in [MCP tools](mcp.md#tools).

`list_quotas` includes current windows and weighted totals, credential/authentication and suspension flags, earliest predicted exhausted account, freshness, rate and budget. Five-hour rate fits at most 256 points within two percentage points; seven-day rate uses cycle consumption. Data older than the greater of 15 minutes or three polling intervals is stale and has no forecast. Seven-day even pace is remaining cycle fraction; daily even budget is `100/7`, with actual remaining, difference and remaining daily allowance. Unknown sliding-window state yields null.

History scanning filters provider/account/window before keeping the latest sample per bucket. `list_quotas` reads only current-window samples. Bucket size is at least the management bucket and range/2000, rounded down to seconds with a one-second floor. More than 20,000 intermediate buckets produces a tool error. Output history is bounded to 200 points per series and 2,000 overall, retaining endpoints; it omits management `resets`. Deleted accounts use `retired account`.

Refresh enters the existing engine command queue. Account selection, running-state recheck and its global 60-second limit are coordinated under `engine.mu`; accepted failures count, while stopped/disabled requests do not. Client cancellation ends waiting but may leave an accepted engine query to finish. Events filter at most 200 retained engine events, with up to 100 output entries; credential events add whitelisted `reason` and `source`. The internal event account index allows identifying credentials that failed before producing a group.

CPA applies model aliases, session affinity, credentials, cooldowns and selectors during execution. The SDK exposes no complete routing snapshot. Model queries therefore return candidate pools with `certain: false`.

## Providers

Providers register in `internal/providers/provider.go` in display order: GPT (`codex`), Claude, Antigravity, Kimi and Grok (`xai`). `Provider` defines `ID`, `Aliases`, `Title`, default `Monitor`/`Ignite`, active `Fetch`, optional passive `Headers`, optional `Profile` and optional `Ignition` protocol/body/candidates/hints/groups. No `Ignition` means the service cannot be ignited.

`Lookup` ignores case and treats underscores as hyphens. Kimi aliases include `kimi-ai`, `kimi.ai` and `kimi.com`; Grok aliases include `x-ai`, `x.ai` and `grok`. `Cred.Provider` stores the canonical ID and `Cred.HostProvider` the host spelling. Canonical configuration keys override aliases regardless of file order.

`Window.Period` comes from provider duration fields. `quota.Kind` classifies periods within 5% of five hours or seven days, or from 27 through 32 days as `30d`, falling back to IDs/labels when period is unknown. `quota.Period` preserves a positive provider duration, including variable month lengths. IDs include `five-hour`, `seven-day` and `window-30d`; other provider windows keep their IDs and use `PeriodLabel`, such as `30d`. Grok's variable monthly period keeps `credits` as a stable ID. Ignition, cooldowns, charts and notifications use window kinds.

Adding a provider requires Fetch matching the Management Center quota request, Period values, Headers if available, Ignition if supported, registry entry and matching service colors in `page.css`. Update the service table, configuration defaults and active/passive/request/model sections in these docs. Add a group to `groupOrder` only when it needs a fixed position.

## Engine and lifecycle

One engine loop serializes quota, ignition and notification state. HTTP cancellation and stream watchdog goroutines do not mutate quota state.

1. Wait about 20 seconds after registration for CPA credentials/executors.
2. Open the data directory and acquire `instance.lock`.
3. Read CPA configuration every 30 seconds to check global/plugin enablement; disabled plugins receive no host notification, so the engine checks itself. Unreadable host configuration is treated as enabled.
4. Process passive readings, reset signals, active polls, ignition, history cleanup and release checks, then save state.
5. Sleep until the next due action, passive reading, command or configuration change.

`usage.handle` parses and queues headers, dropping passive updates when the queue is full. Management commands queue work and wait up to three minutes. Release checking is the exception: it can run in the request goroutine to avoid waiting behind polls/ignition.

Release checks use `NotRllyRn/cpa-pulse` GitHub `releases/latest`, excluding drafts/prereleases. They run initially and every six hours, retrying failures after 30 minutes. Page checks are limited to one attempt per minute, including failures. `releaseMu` permits only one read; page requests return immediately if one is active, while the engine retries later. The request is cancelled when the plugin stops. Disabled plugins keep the last result without checking. Requests use environment proxy or direct access. Numeric versions compare segment by segment; development/prerelease current versions and nonnumeric latest tags do not advertise updates. Failures keep cached results and log at debug level.

`supervise` restarts a crashed loop after 30 seconds. Every goroutine/RPC must recover panics because an unhandled panic terminates CPA. Valid configuration replaces the active configuration; parse failure preserves it and appears in status. Quiesce/shutdown stops the old instance permanently, even though CPA retains its loaded library. The exclusive lock uses Unix `flock` or a Windows nonshared file open; a new instance retries every ten seconds.

## Quota acquisition

### Active queries

| Provider | Request |
| --- | --- |
| Codex | `GET https://chatgpt.com/backend-api/wham/usage`, with `Chatgpt-Account-Id` |
| Claude | `GET https://api.anthropic.com/api/oauth/usage`, with `anthropic-beta: oauth-2025-04-20` |
| Antigravity | `POST .../v1internal:retrieveUserQuotaSummary`, trying three hosts, body `{"project":"<project_id>"}` |
| Kimi | `GET https://api.kimi.com/coding/v1/usages`; uses `api.kimi.ai` when credential domain/base URL/type/provider or runtime ID/filename identifies that variant |
| Grok | `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits`, Grok CLI headers and credential `sub` as `x-userid` |

Headers follow Management Center quota constants. Codex User-Agent matches CPA's executor; Claude matches its default Claude Code fingerprint. Grok uses weekly `currentPeriod` when present, falling back to monthly billing dates only when absent.

Antigravity User-Agent follows CPA: credential `user_agent` from top-level, attributes or metadata takes precedence; `antigravity/` values lose a trailing ` google-api-nodejs-client/...`. Otherwise it uses `antigravity/hub/<version> darwin/arm64`. Hub version comes from `AntigravityHubManifestURL`, cached for three hours, retrying failures after 30 minutes while retaining cached values; initial fallback is `2.23.0`. Manifest reads are lazy and bypass credential proxies. Old CLI-format User-Agents may omit Gemini quota groups.

Tokens come from credential files; CPA refreshes them. A 401 skips the current poll and awaits refresh, with persistent errors handled as credential problems. A suspension code is handled immediately. Non-2xx returns `quota.HTTPError`: display text includes only the first 300 response bytes, while `Body` retains 4096 for diagnosis. HTTP 401 matches `ErrUnauthorized` and stops Antigravity host fallback. Polls align to absolute interval boundaries (`alignedAfter`), so default reads fall on five-minute clock boundaries.

### Network exit

Credential `proxy_url` uses CPA `sdk/proxyutil`; `direct` and `none` mean direct. An invalid value fails without fallback. Otherwise `host.http.do` follows global `proxy-url`, environment proxies, then direct. The plugin opens an HTTP operation and cancels it on timeout because the host callback has no timeout itself. CPA's Management Center `/api-call` ignores environment proxies; the plugin follows model-request routing instead.

### Plans

Account status has a display `plan` and `default_weight` computed by `providers.PlanOf`. Raw plan values persist by auth index under `plans`.

- Codex reads `plan_type` with each quota query. `prolite`/`self_serve_business_prolite` weigh 5, `pro` 10, `promax` 25, free/go 0.1 and other plans 1. Names follow Codex CLI subscription names; unknown values display verbatim.
- Claude reads `GET https://api.anthropic.com/api/oauth/profile` using quota headers. Raw value joins organization type and rate-limit tier. Pro/Team/Enterprise weigh 1; Max 5x weighs 5 and Max 20x weighs 20; unspecified Max defaults to 5. Account booleans provide a fallback without organization type. Pro parsing was tested with an account; Max/Team mappings follow known Claude Code values without live validation.
- Antigravity reads `POST https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist` with `{"metadata":{"ideType":"ANTIGRAVITY"}}`. `paidTier.id` takes precedence over `currentTier.id`, which may remain `free-tier` for paid users. `free-tier` maps to Free; `g1-<name>-tier` supplies the title-cased name. All weights remain 1 because published quota multipliers are unavailable. Pro `paidTier` was verified; absence of `paidTier` on free accounts is unverified.
- Other providers default to weight 1 without detected plans.

Claude/Antigravity profiles are read every 24 hours per account. A changed active window signature triggers rereads for one hour (`planRecheckPeriod`) until a changed plan is seen. Signatures persist so changes while stopped are detected. During failed rereads, accounts with a known plan record quota samples without alerts; accounts never assigned a plan continue normally. Failed profile reads retry after 15 minutes (`planRetryPeriod`); after an hour alerts resume against the old baseline. A plan change that lags window changes may still cause one false quota alert.

`notePlan` runs before alerts. A changed raw value and display name records `plan_change`, stores a pending notice and marks all account windows `rebase`. Rebased windows silently accept the new quota while retaining sent reset-reminder markers and `ended_reset`. Plan notifications use time-sensitive priority and retry for one hour; reverting before delivery cancels both changes. First reads only establish a plan. Passive Claude readings do not fetch a profile and can compare against the preceding plan until the next profile read.

### Passive readings

| Provider | Headers | Interpretation |
| --- | --- | --- |
| Claude | `Anthropic-Ratelimit-Unified-5h-Utilization`/`-5h-Reset`, likewise `-7d-` | Used fraction, e.g. `0.2`, and reset |
| Claude | `Anthropic-Ratelimit-Unified-7d_oi-Utilization`/`-7d_oi-Reset` | Fable seven-day group (`FableGroupKey`); live validation with a Fable-enabled account is pending |
| Codex | `X-Codex-Primary-Used-Percent`, `-Reset-At`, `-Reset-After-Seconds`, `-Window-Minutes`, likewise Secondary | 300 minutes is five-hour, 10080 seven-day; absent duration defaults Primary/Secondary to five-hour/seven-day |

Other providers have no passive quota headers. Active results replace an account's group/window set; passive updates change only reported windows. Empty groups remain to prevent passive resurrection. `groups.active_windows` persists the last successful active set: missing/null means unconfirmed, empty means confirmed absent. Absent window baselines are rebased across restart; only active success can restore a window. Remaining supported windows can still be ignited when the five-hour window is absent; historical series remain.

Old state migrates from a nonempty valid `plans.windows` signature where possible. Empty signatures cannot distinguish unqueried from absent. Unmigratable existing groups reject passive additions until active success, while existing in-memory windows can update. Truly new groups can initially be established passively. Existence is never inferred from alert baselines or ignition observations.

Relative reset headers use usage `RequestedAt`, because records arrive at request completion. `WindowView.ReadAt` is query start for active reads, `RequestedAt` for passive reads and ignition start for confirmation. Passive values older than the current reading are dropped per window; if all are stale, history and alerts are skipped. Active results are not subject to that check.

`shouldSkipActive` never skips providers without passive headers. For others it skips a scheduled read when passive data arrived within `passive_skip_seconds` and the last active read was less than `passive_skip_max_minutes` ago. Manual refresh never skips.

## Ignition

### Schedule

`ignition.windows` enables `short` (5h) and `long` (7d/30d), both by default; an explicit empty list disables automatic pulses. Provider/group enablement and `providers.Pulseable` also apply. Codex supports main 5h/7d/30d windows, Claude its main 5h/7d windows, and Antigravity its configured supported groups. Scoped Claude pools such as Fable have no pulse model mapping. Kimi and xAI stay monitor-only.

`TargetState.Windows` is keyed by observed window ID. Each entry records rolling-reset detection, reset observations, last attempt/success and `PulsedResetEpoch`. Legacy target timing migrates only into a 5h entry. Shared target state retains model selection, cooldown/retry holds and failure protection.

`Schedule.WindowDueAt` applies reset plus grace and daily hours independently to each enabled window. It never substitutes an earlier daily start for a future fixed reset. Short windows may start with an unknown reset; long windows require a known reset. A serviced `PulsedResetEpoch` suppresses repeated requests against the same stale reset. One request per group serves all currently due windows, and each confirms independently. An exhausted window blocks scheduled requests until reset and confirmed recovery, without failure or notification. Status exposes `blocked_window` and `blocked_until`; manual ignition bypasses this gate.

`ignite.ObserveWindow` detects sliding placeholders using each window's period: the reset lies about one period ahead (120-second tolerance) and advances with elapsed time (2% tolerance bounded to 3–10 seconds). Active readings less than two seconds apart preserve the rolling state. Passive readings come from real requests and are never sliding placeholders.

### Requests

Ignition uses `host.model.execute_stream` pinned by runtime `auth_id` and original `forced_provider`. Provider `Ignition.Body` is encoded by `providers.Request`. Kimi fixed windows and Grok automatically renewed periods need no ignition.

| Provider | Protocol | Options |
| --- | --- | --- |
| Codex | `openai-response` | `reasoning.effort: none`, `tools: []`, `stream: true` |
| Claude | `claude` | `stream: true` |
| Antigravity | `openai` | `temperature: 0`, `reasoning_effort: none`, `stream: true` |

Every request omits an output-token cap and uses `providers.TriggerPrompt`:

```text
Reply with only OK. Do not think, do not use tools, do not do anything else.
```

### Streaming and cancellation

`executeStream` synchronously opens a stream, reads chunks by `stream_id` and preserves headers for confirmation. Chat/Antigravity chunks are raw JSON objects. Claude uses standard SSE byte framing, retaining partial lines/events. Codex accepts CPA Scanner's newline-stripped complete lines until any chunk contains a newline, after which that stream permanently uses byte framing. This depends on CPA's Codex framing; newline-free byte fragments would require a parser update.

SSE handles fragmented lines, CRLF and UTF-8, with 1 MiB limits for each line/event and Chat chunk. Nonempty text, visible reasoning or tool arguments trigger first output; heartbeat, role, lifecycle and encrypted reasoning do not.

An independent watchdog closes the host stream two seconds after first output using a monotonic timer. Closing cancels CPA's upstream HTTP context even during blocked reads. Completion, errors, cancellation and plugin stop also release the stream. A mutex arbitrates single terminal ownership, `sync.Once` guarantees one close and calls wait for watchdog exit. A known completion/error cannot be overwritten by late deadlines, cancellation or watchdog panic. A deadline sees already-ready stop/cancellation as cancellation. Close errors retain the execution error as primary. Deliberate cutoff is `output_timeout`, never model fallback, and still requires quota confirmation. EOF before a Claude/Responses terminal event is unexpected; Chat continues after `finish_reason` to capture separate usage before EOF.

Stream/protocol errors become `host.Error` using whitelisted status/code/type fields. Explicit status wins; known rate-limit/authentication/model-not-found codes supply missing 429/401/404. Free message text does not determine status. Explicit stream authentication failures discard local-cooldown markers. Suspension codes/reasons remain whitelisted, with absent status treated as 403. Only approved HTTPS Google appeal URLs are retained. Unknown IDs, raw messages and payloads are discarded. Model switching uses the account/model checks under [Model selection](#model-selection); generic HTTP 403/404, authentication, suspension and rate limits do not switch models.

CPA SDK v8.0.4, checked through v8.0.60, has no public startup-cancellation handle. The host uses `context.WithoutCancel`, waits synchronously for the first raw chunk, then registers the stream ID. A lifecycle chunk is not visible output. Before the ID returns the plugin cannot close the request, so startup and stopping follow CPA's waiting behavior. After it returns, an already-stopped call closes immediately. `request_timeout_seconds` applies to other HTTP requests, not model startup or first output. Startup timeout/cancellation needs a future public host handle; returning from a detached goroutine would leave upstream work running and is unsuitable.

### Call logs

Each model call, including failed startup, writes one structured ignition JSON record in the host log message as well as fields, because CPA's default formatter drops arbitrary fields. JSON escaping prevents injected log lines. Records stay out of UI events, history and state.

Fields include provider, model, auth index, random correlation ID, stream ID, status, reply/truncation/output flags, usage/source/completeness, end reason, first-output wait and total duration. Reply concatenates text, visible reasoning and tool arguments in received order, limited to 4096 Unicode characters without splitting UTF-8. First-output wait is null without output. Full requests, credentials, raw error payloads and opaque reasoning are excluded.

`usage_source` is `cpa_host`; whitelisted counters and breakdowns retain host values without estimating missing tokens. Claude start/delta counters merge. Completeness is `missing`, `partial` or `complete`: only normally completed native Claude/Codex routes with final input/output counters qualify as complete host-protocol accounting. This does not promise every upstream breakdown. Deliberate cutoff and Antigravity/Chat remain partial even after natural completion because CPA may synthesize zeros or merge reasoning counters. End reasons distinguish completion, cutoff, cancellation, startup/HTTP/read/parse errors, unexpected EOF and close failure.

### Model selection

The SDK cannot list models per credential, so `models_api_key` reads the deduplicated union at `/v1/models`. Provider/group compatibility uses model names because `owned_by` can be ambiguous. Selection precedence is account/group `account_models[authID][groupKey]`, provider/group `model`/`models`, then automatic ranking. Manual choices are strict and never fall back; an available CPA list must contain the chosen ID. If listing fails, an explicit model or the last working eligible model can be used.

`pricing.Manager` loads the version-1 `model_prices.json` catalog and `model_price_overrides.json` separately under `data_dir`, with embedded offline entries when no valid catalog exists. The models.dev v1 adapter fetches `https://models.dev/api.json`, supports ETag/Last-Modified validation and keeps usable cached data on error. Refresh is asynchronous and shared across callers, with `pricing.refresh_hours` defaulting to 24 (`0` manual only, `1–168` scheduled) and a one-hour error backoff. Pulses never fetch prices synchronously.

Automatic candidates must match the native catalog provider, support active text generation, have both prices and remain enabled. Deprecated/retired, batch-only, embedding, reranking, moderation, transcription and speech-only models are excluded. Known aliases resolve explicitly; reseller prices are not substituted. Rank by `(32 × input + 2 × output) / 1,000,000` USD, then confirmed model on ties, then model ID. This micro-pulse estimate is only for ordering. If no compatible model has a usable price, use provider candidate ordering while preserving known disabled/deprecated/non-text exclusions.

CPA-local `auth_not_found` and `unknown provider for model` can advance through at most six candidates without consuming the upstream limit. At most two upstream attempts total are allowed. Only a model-specific access/entitlement, unsupported-model or missing-model refusal (including 400/403/404) allows automatic fallback. Generic permission/route errors, authentication, suspension, rate limits and network failures stop the attempt. Deliberate output cutoff never switches models.

Refusals persist in `state.json` per account/group/model for 24 hours, including failed fallbacks, and clear after a credential fingerprint change. Manual choices bypass refusal cooldowns. Successful fallback emits `model_refused`. Target state keeps `LastSuccessfulModel` and `ConfirmedModel`; group status exposes `manual_model`, `estimated_cheapest_model`, `confirmed_model` and `available_models`. CPA model lists are checked at startup, address/key changes and each ignition, with a 10-minute successful-list cache; initial failures retry with polls. `next_models` supplies provider hints, using Gemini for Antigravity.

### Confirmation

A 2xx stream ending naturally or deliberately still needs a fresh fixed future reset for every due window. Both reading and observation timestamps must be at or after the call start; output/usage alone is insufficient. An unchanged long reset cannot confirm an automatic pulse. Each confirmed sibling retains its success and serviced reset even if another window fails. Claude may confirm from response headers; Codex usage is processed while waiting. Otherwise the plugin queries the account on checks separated by 5, 10, 15, 30 and 15 seconds. Antigravity depends entirely on active confirmation. Any unconfirmed due window returns `ErrNotConfirmed`.

### Failure protection

Manual failure records `ignite_failed` with `manual: "1"`, error and summary; it leaves automatic retry, failure and credential state unchanged. Its confirmation queries neither establish nor clear credential problems. Manual success records success and clears failure/credential problems.

| Automatic failure | Handling |
| --- | --- |
| CPA-local model rejection | Try another candidate |
| Model-specific upstream refusal | At most two upstream attempts total for automatic selection; manual choices never switch |
| Diagnosed auth/suspension | Pause until credential change or successful manual ignition |
| High risk: unconfirmed/no model, 400/401/403/404/409/422/429 and unsupported/not-found errors | Pause until next start hour; 429 uses rate-limit notification |
| Other errors, including ordinary CPA cooldown | Retry at `failure_retry_seconds × multiplier^(n−1)`; pause after `max_transient_failures` |

A CPA cooldown with `reset_seconds ≤ 10 minutes` gets one retry after cooldown plus grace without counting failure; a consecutive cooldown counts normally. Pauses notify once per circuit end time. Success clears failures. Quota observations also clear failures when ignition is enabled for the group and every enabled pulseable window has a fixed future reset, confirmed by passive usage or a prior fixed observation at least two seconds earlier. Summaries remain short; full error text stays in event error and status `last_error`.

`checkCooldowns` detects obsolete host cooldowns after polls: more than ten minutes remain, and a quota group has recent (15-minute) unexhausted readings in all windows. It records `cooldown_stale`, exposes `cooldown_until`/`stale_cooldown` and sends once per cooldown end time, persisting `cooldown_notices`. The plugin does not clear routing cooldowns on the user's behalf.

### Credential problems

`failure.Diagnose` extracts status, code/reason, appeal URL and first-sentence summary, limited to 160 characters. Google details reasons take precedence. It can recover a complete message string from truncated JSON. Unreadable messages use a fixed status/code summary (`SummaryFallback`) rather than raw JSON; duplicated status/code text is omitted from credential summaries.

Suspension requires known codes such as `TOS_VIOLATION`/`account_deactivated`, or explicit account/organization suspension wording with 400/401/403. Permission errors and `SERVICE_DISABLED` alone are not suspension. Auth uses 401, `ErrUnauthorized` or authentication/invalid-grant codes. Rate limit uses 429 or `RESOURCE_EXHAUSTED`, excluding CPA-local cooldowns.

Credential state persists by auth index in `credentials`:

- Automatic ignition captures the credential fingerprint before calling; a failed read stays unknown to avoid attributing an in-flight error to a newly written credential. Auth/suspension records a reasoned pause without `until` and blocks all groups, including next-time calculation, until change or manual success.
- Quota suspension is immediate; quota auth requires at least three consecutive failures over 30 minutes, allowing normal CPA refresh. Other results clear the streak.
- A new reason records `credential_invalid` and one notification. Repeated identical diagnoses update the fingerprint without duplicates, including across restart. Ignition-confirmed problems retain confirmation across later query diagnoses unless the credential changed. Delivery retries with polls at most once each poll for one hour; without a delivered problem notice, no recovery notice is sent.
- `Fingerprint` uses the first eight bytes of SHA-256 of refresh token, then API key, then access token. Comparing file timestamps or all contents would treat CPA's routine rewrites as re-login. A new fingerprint resumes scheduled attempts; the same reason failing again pauses without another notice. An unknown fingerprint permits one retry when first readable. Refresh-token rotation can cause an extra attempt after successful refresh.
- Successful ignition, including manual, clears any problem. Successful quota reads only clear problems not confirmed by ignition, since suspended accounts can still answer quota queries. Recovery records `credential_restored` and notifies only when the problem notice was delivered.

Management status copies `credential_problem` under `engine.mu`. Credential events/notices share a stored group key where available, otherwise account label; their internal account index supports MCP association before the first quota group exists.

## Notifications

Configuration, placeholders, examples and channel limits are in [Configuration](configuration.md#webhook).

`Alerts.ProcessGroup` compares each window with its notified baseline. Levels are normal, notice, low, critical and exhausted (≤0.01%). Crossing to worse sends a notice; critical/exhausted is time-sensitive. A genuine recovery needs a new reset cycle or a rise of at least five points (`RecoveryJump`). Small active/passive rounding differences do not reset notification levels. Improvements silently update the baseline so later declines can notify again.

Recovery/reminder modes use window kind; nonstandard windows observed more than a day before reset count as seven-day, otherwise five-hour. Reset times within ten seconds describe one cycle. Two known reset times must change and either pass the old reset or rise by `RecoveryJump` to count as a cycle end; this excludes sliding placeholders. Without a new reset, old-reset passage or the same rise can establish a switch. Late readings from ended/older cycles are ignored.

`notify.Bounced` suppresses temporary returns to an already-ended cycle from changing alert baselines or recording reset/exhaustion events, while retaining history samples. A bounce lasting at least 30 minutes and three readings silently rebases and clears `ended_reset`; a normal reading clears its bounce counters. This avoids duplicate alerts without permanently rejecting an upstream correction.

Cycle switches store `pending_recovery`, previous remaining quota and exhaustion. The prior value is the last pre-reset reading, omitted if older than 15 minutes unless exhausted. An unused 100% cycle sends no recovery. `after_exhausted` only notifies exhausted cycles. Delivery retries for an hour. Recovery and decline are separate messages.

`engine.probes` schedules account queries 30 seconds before/after every known reset regardless of notification modes, merging times within ten seconds and skipping when newer readings exist. Post-reset probes only run within ten minutes of reset. Calculating probes does not mark them run. Reset reminders send once at one hour for five-hour class or one day for seven-day class; `has_remaining` requires more than the critical threshold. New windows only establish baselines. Total delivery failure leaves notified levels unchanged for retry.

Bark uses `GET {bark_url}/{title}/{body}?group&level&icon&url`, percent-encoding all but RFC 3986 unreserved characters. A JSON code other than 200 fails. The default icon points to this repository's `assets/logo.png`; the older Management Center default migrates to it.

### Channel delivery

`applyRuntimeConfig` builds `notify.Fanout` channels. They send concurrently, wait for all results and count delivery when at least one succeeds. Each failed channel records `notify_failed`; successful channels are not retried to prevent duplicates. With no channels, `ErrNotConfigured` silently advances quota alert/reminder state so old alerts are not replayed. Circuit/cooldown notices do not record failures; unexpired reset signals remain eligible. An invalid-only webhook counts as failure and retries.

Webhook validation checks HTTP(S) URL, GET/POST/PUT method, no GET body, legal case-insensitively unique headers without embedded control characters and known placeholders. Invalid settings appear in `notify_error` and test results while other configuration applies. Placeholders use URL percent encoding, JSON escaping, form encoding or plain text according to location/content type; header newlines become spaces. Missing content type is inferred using a placeholder-substituted JSON validity check. Empty body sends built-in ordered JSON. Priority maps time-sensitive to `high`, otherwise `normal`.

Webhook responses are limited to 64 KiB. Non-2xx fails; configured `success_json` additionally requires matching top-level JSON values after JSON normalization, so integer 0 equals response 0.0. Error/test text strips request addresses and redacts URLs, long path/query values, headers and long body strings that may hold tokens.

Notification text is defined in `internal/notify/text.go` and uses English, including fixed `5h`/`7d`/`30d` window names and `month/day weekday hour:minute` times in the configured time zone. Stored user text is not translated.

### Reset signals

Did Codex Reset polling reads ten records on aligned intervals of at least 300 seconds. Nonmanual records deduplicate by ID; `manual:` records by content. At most 100 seen keys and 100 sent schedules persist. Initial records are marked seen, with pending schedules sent only if enabled by `notify_current_pending`.

Only pending schedules whose window has not ended and completions whose latest completion/effective/announcement time is within 48 hours may send. Schedules group by reset kind, window start and scope plans/windows; start alone supports records without a full schedule window. Each poll chooses the latest eligible announcement, breaking ties by record key. Later posts about a sent schedule send an update only when newer and confidence/time/scope changed. Stored comparison content uses UTC English without weekday to preserve deduplication with earlier saved content.

Times use `scheduleWindow`, falling back to `effectiveAt`. Equal endpoints display one time; deadlines display the end as a deadline; date-only windows display a local-time range because they cover a whole day in their source time zone.

## Storage

The default directory is `<plugins.dir>/data/cpa-pulse`. `DefaultDataDir` reads CPA `plugins.dir` (default `plugins`), expands leading `~` and resolves relative paths against CPA's working directory. It cannot use the loaded library path: CPA loads temporary copies under `cliproxy-pluginhost/pid-<pid>/`, which do not survive restarts.

| File | Contents |
| --- | --- |
| `state.json` | `groups`: window baselines/active sets; `scheduler`: shared target and per-window ignition state; `refused_models`: account/group/model refusal cooldowns; `codex_reset_updates`: seen/sent signals; `cooldown_notices`; compatibility `language`; `plans`: raw plans/check times/window signatures/recheck deadlines/notices; `credentials`: fingerprints/problems/notices. Written via temporary file replacement. |
| `model_prices.json` | Version-1 source catalog, validators, timestamps and last refresh error |
| `model_price_overrides.json` | Version-1 custom prices and disabled-model tombstones; unaffected by catalog refresh |
| `history/YYYY-MM-DD.jsonl` | Samples and events partitioned by UTC date |
| `instance.lock` | Exclusive engine instance lock |

Sample records use this shape (timestamps are Unix seconds):

```json
{"k":"s","t":1800000000,"g":"codex:3:codex:main","w":"five-hour","r":80,"x":1800003600,"s":"active"}
```

Event fields are `k: "e"`, `t`, `g` group, `e` type, `v` level, `m` message, `l` label, `d` detail, `p` parameters and optional `a` credential auth index for credential events. Messages/details are log text and may include preserved historical wording. Structured parameters support current page rendering:

| Event | Parameters |
| --- | --- |
| `ignite`, `ignite_manual` | `model`, `reset` |
| `ignite_failed` | Automatic: `retry_seconds`, `error`, `summary`, optional `cooldown`; manual: `manual: "1"`, `error`, `summary` |
| `ignite_paused` | `until`, `error`, `summary`, optional `reason`; auth/suspension omits `until` |
| `credential_invalid` | `reason`, `status`, `code`, `summary`, `appeal_url`, `source`; rate limits can add `until` |
| `credential_restored` | `source` |
| `model_refused` | `model`, `fallback`, `hours` |
| `notify`, `notify_failed` | `title`, `notify_kind`; failure adds `error`; recovery adds comma-separated window IDs in `windows` |
| `cooldown_stale` | `until` |
| `plan_change` | `from`, `to` |
| `codex_reset` | `count` |
| `reset`, `exhausted` | `window`, `remaining`, `kind`, `label`, optional `reset` |

Parameter times use RFC 3339 UTC. Every active sample writes; passive samples write only on value/reset change and at most once per minute per window, buffering interim values in `pendingSamp`.

History buckets retain the last sample: no bucketing through two days, ten-minute buckets above two days, fifteen minutes above four days, thirty minutes above eight days and one hour above fifteen days. The API returns `bucket_seconds`. Each series has `points: [[time, remaining, passive], ...]`, account-bearing `label`, shared `source_label` and independent `resets: [[observation_time, reset_time], ...]` generated before downsampling. Reset changes over 60 seconds are recorded; known/unknown transitions always record, using zero for unknown. A pre-range reset record may precede the one-day point lead and need not be the first observation of that reset. Series never having a reset return an empty reset array.

`History.QueryWindow` scans retained records once, keeping only the latest pre-range predecessor per series and whether a reset was ever known. Equal-time predecessors use the last appended value; out-of-order writes cannot replace a newer predecessor. In-range records are stably sorted before reset extraction, bucketing and events. A predecessor within one day supplies a leading point; older ones only seed reset state. Series without in-range samples are omitted. Memory is `O(series + in-range records)`, without sorting older history or creating disk temporary files. Scan and processing honor cancellation.

Group keys use `<provider>:<auth_index>:<group>`, such as `codex:3:codex:main`, `claude:3:claude:seven-day-fable` and `antigravity:4:antigravity:gemini-models`. The same key is the ignition target ID.

## Design constraints

- Send ignition only through the host model executor and pin the credential.
- Match model-request network routing; fail if the exit cannot be determined.
- Never expose access tokens, notification addresses/headers/bodies, `models_api_key` or `mcp_key` in logs, events, plugin status or state/history files. CPA's configuration remains the authoritative plaintext configuration store.
- Check host JSON fields against the supported CPA `sdk/pluginapi` and `internal/pluginhost` implementation.
- Keep `state.json` backward compatible: missing old fields must use safe defaults.
