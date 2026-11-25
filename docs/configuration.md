# Configuration

[Usage](usage.md) · [MCP](mcp.md)

All plugin settings live under `plugins.configs.cpa-pulse` in `config.yaml`. CPA also requires global `plugins.enabled: true` and the plugin entry's `enabled: true`; CPA owns these enablement switches. On the management page, use the "Quota | Settings" switch at the top to open the settings view. Switches and checkboxes save when clicked; text and numbers save when you leave the field or press Enter. Save results appear to the right of section headings, or beside each group legend under "Advanced". The three alert thresholds save together and must be in descending order, with equal values allowed. Changes apply without a restart. A saved field shows "Saved; waiting to take effect", then "In effect" after the plugin confirms it; confirmation waits at most 15 seconds after the save succeeds, even if status requests hang. If still unconfirmed at the deadline, it shows "Not confirmed in effect"; a late confirmation does not change that wait result.

With no failures, the heading shows the most recent save's progress. A failed save takes priority, with the field name and reason; failed controls have red borders and keep their input drafts. Saving another item cannot hide the failure; it clears only when the failed item saves successfully. For multiple failures, the latest reason is shown with the number of other failures. On narrow screens, only "Save failed" and the failure count remain visible; field names and reasons are hidden to keep long text out of the heading. Editing revokes older requests' ability to update that item's result, so late responses and confirmations cannot overwrite later edits. Unresolved failures remain visible. MCP settings also show why the endpoint failed to start.

| Key | Default | Description |
| --- | --- | --- |
| `bark_url` | empty | Bark push URL up to the device key; empty disables Bark |
| `bark_group` | `CPA` | Bark notification group |
| `bark_icon` | cpa-pulse logo | Notification icon |
| `webhook` | empty | Generic webhook, see [Webhook](#webhook) |
| `notice_threshold` | `50` | First alert level (remaining percent) |
| `low_threshold` | `20` | Second alert level |
| `critical_threshold` | `10` | Third alert level |
| `recovery_notify` | 5-hour `after_exhausted`, 7-day `all` | Recovery notifications, see below |
| `reset_reminder` | 5-hour `off`, 7-day `has_remaining` | Reset reminders, see below |
| `timezone` | empty | Time zone for displayed times and the ignition window, such as `Europe/London`; empty follows the CPA server (set by `TZ` in the official Docker image) |
| `timezone_offset_hours` | empty | Fixed UTC offset from −14 to +14, including fractions; used when no valid named time zone is available |
| `poll_interval_seconds` | `300` | Active query interval, at least 60 |
| `request_timeout_seconds` | `20` | HTTP timeout in seconds, clamped to 5–120; does not limit ignition startup or first output |
| `passive_skip_seconds` | `60` | Skip the query when passive data arrived this many seconds before it; `0` never skips |
| `passive_skip_max_minutes` | `30` | Longest run of skipped queries for one account |
| `models_api_key` | empty | CPA API key for reading the model list; without it ignition cannot pick a model. The management key does not work here; the page does not save the management key or a key CPA does not accept, and says why |
| `cpa_base_url` | `http://127.0.0.1:8317` | Address the plugin uses to reach CPA; change it when CPA uses another port or TLS |
| `history_retention_days` | `40` | Days of quota history to keep, clamped to 1–365 |
| `remember_chart_filters` | `false` | "Remember the window, range and order" on the page: the management page remembers, in the current browser, the quota window, time range and order last chosen in the quota chart; when off, the page opens with the defaults |
| `remember_chart_expanded` | `false` | "Remember expanded groups" on the page: the management page remembers, in the current browser, the multi-account groups expanded in the quota chart, one shared set for both the 5-hour and 7-day quotas; when off, the page opens with the groups collapsed |
| `account_weights` | empty | The weight of each account in the total of several accounts, keyed by credential file name, a number above 0; accounts without one use the [plan default](usage.md#features) |
| `account_order` | empty | Account order by credential file name; unlisted accounts follow in default order and missing entries are ignored |
| `mcp_enabled` | `false` | Enable the independent MCP server, see [MCP](mcp.md) |
| `mcp_listen` | `127.0.0.1:8319` | MCP listen address |
| `mcp_key` | empty | Dedicated MCP Bearer key; empty prevents listening |
| `config_token` | empty | Configuration acknowledgment token generated on each page save; usually no manual setting is needed. See [Architecture](architecture.md#cpa-interface) |
| `data_dir` | `data/cpa-pulse` in the plugin directory | State and history directory |

A valid IANA `timezone` takes precedence over a fixed offset; an invalid name falls back to the offset or CPA process time zone.

Alert thresholds are normalized to `100 ≥ notice_threshold ≥ low_threshold ≥ critical_threshold ≥ 0`: later levels cannot exceed earlier ones. The normalized values take effect and the adjustment is logged.

With both `bark_url` and `webhook.url` empty, notifications are off: nothing is sent and no failed notifications are logged as events. Quota alerts from that time are not sent once a URL is set; Did Codex Reset signals that have not expired are still sent.

## Recovery and reset reminders

Recovery notifications and reset reminders are set per window: `five_hour` for 5-hour windows and `seven_day` for 7-day windows.

| Key | Values | Description |
| --- | --- | --- |
| `recovery_notify` | `off`, `all`, `after_exhausted` | `all`: notify every time the window resets; `after_exhausted`: after the window runs out, notify once at its next reset |
| `reset_reminder` | `off`, `all`, `has_remaining` | 5-hour windows are reminded 1 hour before the reset, 7-day windows 1 day before; `has_remaining`: remind only when more than `critical_threshold` is left |

The first line of a recovery notification is what was left when the previous cycle ended. This value comes from the active query 30 seconds before the reset; usage in those last 30 seconds is not counted. Without a reading from the last 15 minutes before the reset, the line is left out. A cycle that was never used (still 100% before the reset) is not notified. A window that is neither a 5-hour nor a 7-day window counts as a 7-day window once it was seen more than a day before its reset, and as a 5-hour window otherwise.

`notify_recovery` and `notify_reset_reminders` set both windows of the matching key to `all` when `true` and to `off` when `false`. They apply only when `recovery_notify` or `reset_reminder` is absent; changing the matching notification setting on the management page replaces them with the new keys.

## Ignition

Ignition settings live under `ignition`:

| Key | Default | Description |
| --- | --- | --- |
| `enabled` | `true` | Enable window ignition |
| `start_hour` | `7` | Hour of the first ignition each day |
| `end_hour` | `22` | Hour at which ignition stops |
| `end_grace_minutes` | `30` | Minutes after `end_hour` that are still allowed |
| `grace_seconds` | `3` | Seconds to wait after a reset |
| `failure_retry_seconds` | `300` | Wait before the first retry after a transient error |
| `failure_backoff_multiplier` | `3` | Factor applied to the wait on each retry |
| `max_transient_failures` | `3` | Transient errors that pause ignition until the next day |
| `post_success_hold_seconds` | `60` | Minimum wait after a successful ignition |

Hours are clamped to 0–23; grace minutes/seconds to zero or more. Retry seconds are at least 60, retry multiplier and maximum failures are clamped to 1–10, and post-success hold is at least 15 seconds. Passive skip settings accept zero or more; a zero maximum disables the skip window. Invalid notification modes become `off`. Invalid or nonpositive account weights are discarded.

## Providers

All five providers are monitored by default. Automatic ignition defaults on for Codex and Claude, off for Antigravity, and unsupported for Kimi and Grok. Models default to automatic selection; per-group model overrides default empty.

Per-service settings live under `providers`, keyed `codex`, `claude`, `antigravity`, `kimi` and `xai` (Grok). CPA can name Kimi accounts `kimi-ai` and similar; any of these keys counts as `kimi`. Kimi and Grok cannot be ignited, so `ignite` has no effect for them:

| Key | Description |
| --- | --- |
| `monitor` | Monitor this service |
| `ignite` | Ignite this service |
| `model` | Ignition model; empty picks one automatically. It must appear in CPA's model list |
| `groups` | Antigravity only: quota groups to ignite, default `["Gemini"]` |
| `models` | Antigravity only: model per quota group, for example `{"Gemini": "gemini-3.5-flash-lite"}` |

## Reset signals

`codex_reset_updates` controls Did Codex Reset forwarding: `enabled` (default `false`), `poll_seconds` (default `300`, at least 300) and `notify_current_pending` (default `true`: on first enable, notify the pending scheduled reset). Only schedules that have not passed and resets completed in the last 48 hours are forwarded, and a later post about a schedule is sent as an update only when its confidence, time or plans change; schedule times are shown in the local time zone, and date-only schedules as a time range. Did Codex Reset is a third-party monitor; its signals are not official OpenAI announcements.

Example configuration:

```yaml
plugins:
  configs:
    cpa-pulse:
      enabled: true
      bark_url: "https://api.day.app/your_device_key"
      models_api_key: "new key for cpa-pulse"
      ignition:
        enabled: true
        start_hour: 7
        end_hour: 22
      providers:
        codex:
          ignite: true
        claude:
          ignite: true
        antigravity:
          ignite: true
          groups: ["Gemini"]
      codex_reset_updates:
        enabled: true
```

## Notification example

Notifications use English. Existing stored notification and event text is preserved verbatim.

```text
🟡 Claude · 7d 48% | 03d

5h: 96% | 04h | 10/04 Sun 13:50
7d: 48% | 03d | 10/07 Wed 14:00
```

Title icons: 🟡 remaining fell to the first threshold (50% by default), 🔴 to the second threshold (20% by default) or below, ✅ recovered, ⏰ reset reminder, 🎫 plan changed, 🔑 credential invalid, ⛔ account suspended, ⏳ rate limited, ✅ credential works again, ⚠️ a problem that needs action, such as ignition paused or a CPA cooldown that outlasts the quota. Each body line shows the window, the remaining quota, the time until reset, and the reset time. Dates and times in notifications read "month/day weekday hour:minute", such as `10/04 Sun 13:50`. The first time the plugin sees a quota window it only records the current level and sends nothing.

## Webhook

Besides Bark, you can set up one webhook to send notifications to Feishu, ntfy, DingTalk, Telegram and other services, or to your own program. With both channels set, every notification goes to both; it counts as delivered as soon as one of them accepts it and is not retried, and the channel that failed is logged as a "Notification failed" event.

| Key | Default | Description |
| --- | --- | --- |
| `url` | empty | Request URL; empty disables the webhook |
| `method` | `POST` | `GET`, `POST` or `PUT` |
| `headers` | empty | Request headers, such as `Authorization: Bearer <token>` |
| `body` | empty | Request body; empty sends the built-in JSON message. Not allowed with `GET` |
| `success_json` | empty | Success condition: these top-level fields of the JSON response must have the given values, such as `code: 0`; empty only requires a 2xx HTTP status |

The URL, headers and body can use placeholders:

| Placeholder | Content |
| --- | --- |
| `{{title}}` | Notification title |
| `{{body}}` | Notification body |
| `{{text}}` | Title and body, separated by a line break |
| `{{url}}` | Link: the history page for Did Codex Reset notifications, the https appeal link for account suspension notices, empty otherwise |
| `{{priority}}` | `high` when quota drops to the third threshold or runs out, the plan changes, ignition pauses, a credential is invalid, an account is suspended or rate limited, or a CPA cooldown is not cleared; `normal` otherwise |
| `{{kind}}` | Notification type: `quota` quota drop, `recovery` quota recovered, `reminder` reset reminder, `plan` plan changed, `cooldown` CPA cooldown not cleared, `circuit` ignition paused, `credential` credential invalid, `suspended` account suspended, `rate_limited` rate limited, `credential_ok` credential works again, `codex_reset` Did Codex Reset, `test` test notification |
| `{{label}}` | Quota group, such as `GPT#rk` or `GPT#main`; empty for notifications not about a quota group |
| `{{time}}` | Time sent, in RFC 3339 format and the plugin time zone |

Placeholders are escaped for where they appear: URL-encoded in the URL; JSON-string-escaped in a JSON body, so put them inside quotes; form-encoded in a form body; with line breaks replaced by spaces in headers. Without a `Content-Type` header, a body that is JSON (with placeholders inside quotes) is sent as JSON and anything else as plain text.

Without `body`, the webhook receives this JSON:

```json
{"source":"cpa-pulse","kind":"quota","title":"🔴 GPT#rk · 5h 8% | 03h","body":"5h: 8% | 03h | 10/10 Sat 19:00\n7d: 60% | 04d | 10/14 Wed 09:00","text":"…","url":"","priority":"high","label":"GPT#rk","time":"2026-10-10T16:00:00+08:00"}
```

Settings for common services follow. They are format examples based on each service's documentation and have not all been tested.

| Service | `url` | `body` | `success_json` |
| --- | --- | --- | --- |
| ntfy | `https://ntfy.sh/` | `{"topic":"<topic>","title":"{{title}}","message":"{{body}}"}` | not needed |
| Feishu / Lark | `https://open.feishu.cn/open-apis/bot/v2/hook/<token>` | `{"msg_type":"text","content":{"text":"{{text}}"}}` | `code: 0` |
| DingTalk | `https://oapi.dingtalk.com/robot/send?access_token=<token>` | `{"msgtype":"text","text":{"content":"{{text}}"}}` | `errcode: 0` |
| WeCom | `https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=<key>` | `{"msgtype":"text","text":{"content":"{{text}}"}}` | `errcode: 0` |
| Discord | `https://discord.com/api/webhooks/<id>/<token>` | `{"content":"{{text}}"}` | not needed |
| Slack | `https://hooks.slack.com/services/<path>` | `{"text":"{{text}}"}` | not needed |
| Telegram | `https://api.telegram.org/bot<token>/sendMessage` | `{"chat_id":"<chat_id>","text":"{{text}}"}` | not needed |

For Feishu, `config.yaml` looks like this. The body contains `{{`, so put it in single quotes:

```yaml
plugins:
  configs:
    cpa-pulse:
      webhook:
        url: "https://open.feishu.cn/open-apis/bot/v2/hook/<token>"
        body: '{"msg_type":"text","content":{"text":"{{text}}"}}'
        success_json:
          code: 0
```

- Feishu, DingTalk and WeCom answer HTTP 200 when a message fails too, with the error code in the response, so set `success_json`; otherwise failed deliveries count as successful.
- Signature verification of Feishu and DingTalk bots is not supported. Use a custom keyword or an IP allowlist in the bot's security settings instead; with a keyword, put it in the body, such as `"text":"cpa-pulse {{text}}"`.
- "Send test" on the management page shows the result of each channel and the start of the webhook response. Without `success_json`, the page notes that only the HTTP status was checked, so confirm that the message arrived.
- When the webhook settings are invalid, for example a misspelled placeholder, the webhook stays off and the management page shows the reason at the top; the other settings still apply.

