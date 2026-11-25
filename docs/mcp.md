# MCP

[Usage](usage.md) · [Configuration](configuration.md)

MCP lets coding agents query quota through standard Streamable HTTP, independently of the client. The plugin listens on `127.0.0.1:8319` at `/mcp` and requires a dedicated Bearer key. The CPA management key does not authenticate this endpoint. Protocol versions `2025-03-26` and `2025-06-18` are supported; responses are JSON only, without SSE.

Enable MCP in the settings view; if no key is set, the page generates one in the same save. You can also generate a new key separately or configure these keys under `plugins.configs.cpa-pulse`:

```yaml
mcp_enabled: true
mcp_listen: "127.0.0.1:8319"
mcp_key: "<dedicated MCP key>"
```

Use a cryptographic random generator for the key, for example 32 random bytes encoded as base64url with an `llmcp_` prefix. An empty key prevents the listener from starting and reports the reason in management status. Changes to the switch, address or key apply automatically. Status uses `config.mcp_key_set` only to indicate whether a key is set. The top-level `config_applied_token` confirms completion of configuration application; check the listener state and error to determine whether the endpoint is usable. The original key is stored in CPA configuration and can be read by anyone with the management key. In Docker, the loopback address belongs to the CPA container. A local agent can share that network namespace; other deployments should connect a reverse proxy through a restricted container network without publishing an unencrypted MCP port.

## Client connections

The default URL within the same network namespace is `http://127.0.0.1:8319/mcp`. Replace it with an HTTPS URL for remote access. `<key>` below is the dedicated MCP key.

The MCP section of the Settings view builds client configs for Claude Code, generic JSON and mcp-remote from the listen address, ready to copy. When clients go through a reverse proxy, enter its address under "Public address (optional)" and the configs follow it; the address is kept in the current browser only. The public address must be an http or https address, and `https://` is assumed when no scheme is given. The host may contain only ASCII letters, digits, `.` and `-`, without bracketed IPv6 addresses, and the whole address only printable ASCII, without spaces, `?`, `#` or `\`. An invalid address is pointed out on the page, and the configs keep the listen address.

Claude Code:

```sh
claude mcp add --transport http cpa-pulse http://127.0.0.1:8319/mcp --header "Authorization: Bearer <key>"
```

Codex CLI, with the key provided through an environment variable:

```sh
export CPA_PULSE_MCP_KEY='<key>'
codex mcp add cpa-pulse --url http://127.0.0.1:8319/mcp --bearer-token-env-var CPA_PULSE_MCP_KEY
```

Generic client configuration example; check the client's documentation for its configuration format:

```json
{"mcpServers":{"cpa-pulse":{"type":"http","url":"http://127.0.0.1:8319/mcp","headers":{"Authorization":"Bearer <key>"}}}}
```

Clients that only support stdio can connect through `mcp-remote`:

```sh
npx mcp-remote http://127.0.0.1:8319/mcp --header "Authorization: Bearer <key>"
```

## HTTPS reverse proxy

Do not expose the listener directly to the public Internet. For remote use, keep MCP on loopback and terminate TLS with a reverse proxy on the same host. Add firewall restrictions or an allowlist as needed. Preserve the `Authorization` and `MCP-Protocol-Version` headers. Browser requests with a non-local `Origin` are rejected; ordinary MCP agents usually omit this header.

Nginx example; replace the domain and certificate paths:

```nginx
server {
    listen 443 ssl;
    server_name quotas.example.com;
    ssl_certificate /etc/letsencrypt/live/quotas.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/quotas.example.com/privkey.pem;
    location = /mcp {
        proxy_pass http://127.0.0.1:8319/mcp;
        proxy_set_header Authorization $http_authorization;
        proxy_set_header MCP-Protocol-Version $http_mcp_protocol_version;
        proxy_read_timeout 190s;
    }
}
```

Caddy example; point the domain at the proxy and let Caddy obtain the HTTPS certificate:

```caddyfile
quotas.example.com {
    reverse_proxy /mcp 127.0.0.1:8319 {
        transport http {
            response_header_timeout 190s
        }
    }
}
```

## Tools

| Tool | Purpose |
| --- | --- |
| `list_quotas(provider?, account?, model?)` | Current windows, account plans and weights, weighted remaining quota, earliest account exhaustion, resets, forecasts, hourly consumption, seven-day even-pace budgets, freshness and source, credential authentication errors, account suspension and next ignition |
| `get_quota_history(provider?, account?, window?, range?)` | Quota history including retired windows; at most 200 points per series and 2000 points per response |
| `refresh_quotas(account?)` | Active query, globally limited to once per 60 seconds; returns remaining wait seconds when throttled and waits at most 180 seconds for results |
| `get_quota_events(since?, limit?)` | Recent reset, exhaustion, ignition success/failure, and credential problem (invalid credential, suspended account, rate limit) and recovery events; `since` is an RFC3339 timestamp with timezone and `limit` an integer from 1 to 100 |

`get_quota_history` accepts the [management history ranges](architecture.md#cpa-interface), including `31d`, `61d`, `90d` and `151d`, with a default of `24h`. `window` accepts `5h`, `7d`, `30d` or an exact window ID. History retention limits the available data.

`list_quotas` forecasts `7d` and `30d` windows using this cycle's average consumption; `5h` windows use a recent consumption rate.

Each MCP server instance executes at most four tool calls concurrently, including calls within a batch. Additional calls immediately receive a JSON-RPC `-32000` busy error and may be retried later. Query tools have a 10-second timeout; `refresh_quotas` has a 180-second timeout.

MCP account labels use the first line of the note when present, first masking complete emails of all known accounts, then masking every remaining non-whitespace segment containing `@`. Quotes are removed from the user name before keeping its last two characters. Without a note name, the label is `***` plus the last two characters of the email user name, for example `***rk`; shorter user names keep all their characters. The domain and user-name length are not returned. Accounts without a note name or email use `account-number`. Labels that are identical after masking receive `#1`, `#2` and further suffixes, avoiding every base label and previously generated label so that each complete label identifies one account. `retired account` is reserved for retired accounts; a current account with that base label also receives a suffix. Current quotas, earliest account exhaustion, history and events all use these labels and never return full emails. This rule is independent of the page's blur switch.

`account` accepts an exact complete label, the base label before the suffix, the original first note line or the full email. Complete labels take priority and select one account. Shared base labels or note names select multiple accounts, which `refresh_quotas` rejects; use a complete label or a unique email instead. `account="retired account"` selects only retired-account history; use the complete suffixed label or a unique email for a current account with that name. Omitting `account` selects all accounts.

After CPA starts, the engine waits about 20 seconds before its first full poll. While the engine is not running or that poll is incomplete, the three read tools return `ready: false`, `state: "starting"`, `certain: false` and `retry_after_seconds: 30`, with an explanatory note and no data. This is not a tool error. A failed credential-list read allows the read tools to return results instead of waiting indefinitely for startup. When the plugin is disabled in the configuration, tools return `ready: false`, `state: "disabled"` and `certain: false`. `refresh_quotas` only requires a running engine and need not wait for the first full poll; non-running or disabled responses do not consume its 60-second refresh limit. Accepted refreshes on a running engine count toward the limit even if they fail.

Times include both ISO timestamps with timezone and relative descriptions. Forecasts depend on continued use at the measured rate and are not guarantees; stale or unknown predictions are `null`. A `model` query returns candidate quota pools with `certain: false`: CPA also applies model aliases, credential availability, cooldowns and session routing during actual requests, so the plugin cannot guarantee which account will be selected. After a 429, refresh first and use the actual reset and CPA cooldown times to schedule a retry rather than relying only on a forecast. Events are filtered from at most 200 retained engine events of all kinds.

