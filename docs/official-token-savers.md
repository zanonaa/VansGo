# Official Token Saver runtimes

## Delivered

- PXPIPE: npm `pxpipe-proxy@0.15.1`, bundled at build time with npm lockfile integrity verification. `PXPIPE_PACKAGE_ROOT` selects the read-only bundle; dashboard install becomes an availability check, never a runtime download. Native/non-Docker installation pins the same version. Existing start/stop behavior remains.
- Ponytail: optional `official` level embeds the upstream compact `AGENTS.md` without modification, at commit `09c434ab73ca43761c5d8b0c5d341926c2fba710`, with MIT license. Legacy levels remain for compatibility; this does not install client hooks/commands.
- Headroom: official `headroom-ai[proxy]==0.41.0` now embedded in the vansgo image itself. During the image build, `docker/headroom/requirements.txt` (the same uv-generated, hash-pinned lock that the former sidecar used) is installed into `/opt/headroom-venv`, and `/usr/local/bin/headroom` links into that venv. There is no separate container, compose overlay, or internal network anymore: `compose.token-savers.yml` was removed. A Go supervisor (`backend/internal/http/handlers/headroom_process.go`, mirroring upstream `src/lib/headroom/process.js`) spawns, health-waits, stops and pidfile-tracks the proxy on `127.0.0.1:8787` when the dashboard toggle is on — and boots it automatically on gateway start. Sidecar env parity is preserved (`HEADROOM_ALLOW_UNAUTHENTICATED_BIND`, `HEADROOM_COMPRESS_ALLOW_REMOTE`, beacon/telemetry off). Spawn flags follow installed extras: without `ml` packages Kompress stays `--disable-kompress`; `--code-aware` is only passed when the `code` extras exist. Keeping extras installed at runtime with `pip` is possible but intentionally not hash-locked by this repository — document any manual extras separately if you use them. An external Headroom via `headroomUrl` keeps the upstream EXTERNAL_PROXY behavior.

Enable it: turn on the Headroom toggle in Token Saver settings and restart the container (the first boot removes legacy sidecar URLs — `http://headroom:8787`/`http://vansrouter:8787` — from settings, rewriting `headroomUrl` to the loopback default). Dashboard start/stop works from any origin with a session; the previous local-only 403 no longer applies because there is no trusted sidecar network anymore — session/CLI-token auth from the general middleware covers these routes.

## RTK: official CLI via stdin

The gateway now invokes official Apache-2.0 RTK CLI 0.46.0 (`rtk pipe`) on tool-result text before sending the final request. This works with OpenAI Chat Completions and Responses because filtering is independent of the provider endpoint. The Go code only traverses message structures; copied Go filters have been removed. Autodetection and filtering belong entirely to the upstream CLI; unsupported text remains unchanged. No shell commands from messages are executed. Each eligible result has a 2-second timeout and missing/failed CLI leaves the original text with a warning, never a legacy-filter fallback.

Docker downloads the official CLI 0.46.0 release artifacts for AMD64/ARM64 with fixed SHA256 verification. The runtime uses Debian Trixie/glibc because the upstream ARM64 binary requires GLIBC_2.39 (Alpine gcompat and Debian Bookworm were tested and are incompatible). No Rust compiler is used. This avoids the fixed upstreams and separate BUSL license of RTK Proxy. The CLI license is included in the image. Native installations must install `rtk` on PATH.

Ponytail has one official level only. Migration 011 rewrites persisted level settings to `official` without changing any enable flag. Previous prompt variants and runtime aliases have been removed.

## Validation and limits

Local AMD64 runtime validation: Headroom health 200 (`rust_core=loaded`) and `/v1/compress` 200 with the adapter request shape; PXPIPE official exports and synthetic transform (`below_min_chars`). These are not savings benchmarks. No paid model generation is used. ARM64 runtime execution must be validated separately from Go cross-compilation. Versions are intentionally not `latest`; update manifests and locks together.

## Deployment note (upgrade from the sidecar era)

Installations that still carry `compose.token-savers.yml` and a `headroom` service should drop the overlay and the `HEADROOM_URL` env var: the single compose file only defines `vansgo`. The embedded build adds roughly 0.9 GB on disk and ~30 MB Python baseline; per-request memory only grows while the proxy runs. Old images remain a rollback path as long as the old settings URL is restored (or reset to the sidecar service name before rolling back).
