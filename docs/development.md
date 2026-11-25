# Development

[Usage](usage.md) · [Architecture](architecture.md)

## Requirements

- Go 1.26, matching CPA v8.
- cgo and a target C compiler for shared-library builds: Linux uses `golang:1.26-bookworm` to match the official CPA image's Debian 12 glibc; macOS uses Xcode clang; Windows uses MinGW-w64 gcc.
- Node.js for management-page checks.
- Docker for the local build script.

Tests under `internal/` are pure Go and run without cgo on Linux, macOS and Windows. Only the root cgo entry point requires it.

## Checks

```sh
go vet ./internal/...
go test ./internal/...
go test -race ./internal/...
node --check internal/web/page.js
node --test internal/web/page_test.cjs
```

Set `CPA_PULSE_WEB_FIXTURE_DIR` to a directory when running `go test ./internal/engine -run TestWebPreviewFixture` to export management-page fixtures.

Tests use fake hosts and HTTP servers without real model requests. Coverage includes provider parsing and aliases, periods, egress, schedules, model candidates/fallback, stream parsing/cancellation/ownership, usage accounting, alerts, webhook escaping/redaction, reset-signal deduplication, engine state and history, credentials/plans, MCP protocol/data/cancellation, plugin RPC, release comparison and instance locking.

The [CI workflow](../.github/workflows/ci.yml) runs on main pushes and pull requests. Linux checks `gofmt -l .` is empty, then runs vet, Go tests and the JS syntax check. Race and Node behavioral tests are available for local verification. Commits affecting only Markdown, `docs/` or `assets/` do not trigger CI. A newer run cancels the older run on that branch. macOS and Windows Go checks run during release builds.

## Local build

Build for the Docker host's CPU architecture:

```sh
sh scripts/build.sh 0.1.0
```

Output is `dist/cpa-pulse-v0.1.0.so`. CPA reads plugin ID/version from the `-v<version>` filename convention.

## Releases

The release repository is [NotRllyRn/cpa-pulse](https://github.com/NotRllyRn/cpa-pulse/releases). Pushing a `v*` tag runs the [release workflow](../.github/workflows/release.yml):

1. Test each platform before building. Linux checks formatting, vet and tests. macOS/Windows run vet, tests and the JS syntax check; the Linux build container has no Node.js.
2. Run `scripts/ci-build.sh` for five platforms: Linux amd64/arm64 in Debian 12 Go containers on matching Ubuntu runners, macOS amd64/arm64 in one Apple-silicon runner job (clang cross-compiles amd64), and Windows amd64.
3. Package `cpa-pulse_<version>_<os>_<arch>.zip`, with only `cpa-pulse-v<version>.<so|dylib|dll>` at the archive root.
4. Create a draft GitHub Release with all five archives, SHA-256 `checksums.txt` and generated commit notes.

Replace generated notes with concise user-facing release notes before publishing the draft. Drafts are private and unavailable to the CPA plugin store. Store installation expects all five platform archives and `checksums.txt`, and checks SHA-256 before installation.

Tag `v0.1.0` supplies version `0.1.0` through `-ldflags "-X main.version=..."`, displayed in CPA and the plugin page. Manually dispatching the workflow tests/builds and uploads unpackaged Actions artifacts without creating a Release; its version is `0.0.0-dev.<first-seven-commit-characters>`.

## Change conventions

- Check CPA `sdk/pluginabi`, `sdk/pluginapi` and `internal/pluginhost` before changing callbacks, RPC fields or registration. Keep the dependency compatible with the minimum supported host version.
- When updating the CPA dependency, compare Codex and Claude User-Agent constants with CPA's runtime executors. The plugin cannot read the host's compiled constants at runtime.
- Verify quota request URLs/headers against the Management Center `src/utils/quota/constants.ts` and update [Active queries](architecture.md#active-queries).
- Update relevant tests and [Usage](usage.md), [Configuration](configuration.md) and [Architecture](architecture.md) when polling, scheduling, skip rules, failure behavior or configuration changes.
- Recover panics in background goroutines and wait for cancellation watchdogs to exit before returning. HTTP requests need timeouts. Model startup cancellation is constrained by the host SDK; see [Streaming and cancellation](architecture.md#streaming-and-cancellation).
- Keep page HTML, CSS and JS separate. Insert text with `textContent`. Maintain English UI and notification text while preserving user-supplied text and historical data. Bars and charts must share the same quota-level colors.
- Document current user-visible behavior in `docs/`; keep the root README limited to its short description, documentation link and screenshots. Preserve upstream attribution and AGPL notices; see [License and attribution](usage.md#license-and-attribution).

## Live validation

Ignition sends real model requests and starts quota windows. For the first deployment, set `ignition.enabled: false`, verify quota queries, passive readings, management page and notifications, then enable ignition when ready.
