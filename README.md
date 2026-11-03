# VansGo

**VansGo** is a self-hosted AI gateway written in Go: one OpenAI-compatible endpoint in front of every provider, with a web dashboard for connections, API keys, combos (failover routing), quota tracking, proxy pools and token savers.

[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

You connect the providers you actually use (Claude Code, Codex CLI, Grok CLI, GLM/Z.ai, Gemini/Antigravity, OpenCode Free, OpenAI-compatible endpoints …), issue your own API keys, and point any client to `http://localhost:20128/v1`. VansGo translates requests per provider, keeps OAuth tokens refreshed, records usage and cost estimates, and shows live quota bars when the upstream exposes them.

## The original project

VansGo started as a full rewrite of **[Vanszs/VansRouter](https://github.com/Vanszs/VansRouter)** — a Node.js/Next.js AI gateway from the 9Router ecosystem. The original product shipped a Next.js dashboard together with a set of JavaScript executors (`open-sse/`) that proxied each provider's CLI or HTTP API, translating chat-completions traffic into whatever shape the upstream expected and merging upstream responses back to OpenAI-compatible SSE.

That project's migration could move in two plausible directions over time, and VansGo picked **"rewrite in Go"**: one static binary replaces the Node runtime everywhere, while the executors' request/response transformations were ported executor by executor, validated against the original JavaScript behavior with regression tests (`frontend/tests/reference/` keeps the legacy transformers used only as test oracles — they are never imported into the app).

## Why Go

- **One binary.** A single Go binary serves the SPA and the API with no Node at runtime — except where an official component requires it (PXPIPE runs its own package in a subprocess; Docker keeps Node in the image for that alone).
- **Much smaller footprint.** The Go runtime uses a fraction of the memory of the Next.js server, which matters a lot on small ARM boards and shared VPSes.
- **Real concurrency.** Provider calls, SSE streaming, WireGuard userspace tunnel loops and background schedulers (OAuth proactive refresh, todo sweeps) run natively with race safety (`go test -race` is part of the local validation battery).
- **Cross-compilation.** `GOOS=linux GOARCH=arm64` builds are trivial — the Dev environment runs on ARM64 and the CI publishes both AMD64 and ARM64 images.
- **Simpler ops.** One binary + SQLite. Backups are file copies; deploys are image pulls.

## Features

- **OpenAI-compatible gateway**: `/v1/chat/completions` (JSON and SSE), `/v1/models`, `/v1/embeddings`, audio transcriptions, video generation and more, translated per provider.
- **Combos**: ordered lists of providers/combos with automatic failover on credential, quota or upstream errors.
- **Dashboard**: connections, API keys with ACL prefixes, usage and cost history, live `requestDetails`, token saver settings, proxy pools and NordVPN tunnel management.
- **Usage tracking**: records tokens reported by upstreams (including cache/reasoning fields), computes costs from price tables/overrides, and marks estimated values as `estimated` when the upstream omits usage. Old un-recorded consumption is never reconstructed or invented.
- **Provider identity diagnostics**: per-attempt `opencode_identity` audit (opt-in via `OUTBOUND_AUDIT=true`) to attribute outbound identities without rotating or spoofing anything.
- **Proxy pools**: HTTP/SOCKS5 pools with health probing and fitness scoring; selection state per connection.
- **NordVPN tunnels**: userspace WireGuard tunnels persisted in `nordvpnTunnels` (migration v10); startup restores them, and pools go live only after IP verification.
- **Token savers** (official bundled components): RTK CLI 0.46.0 (`rtk pipe`), Headroom 0.41.0 embedded in the image (Go-managed child process), PXPIPE 0.15.1, Ponytail. See `docs/official-token-savers.md`.
- **Security**: deny-by-default on `/api/*`, JWT HS256 session cookie (HttpOnly), API keys (`sk-…`) for remote LLM routes, `x-9r-cli-token` for the local CLI, `LocalOnlyGuard` on `/api/auth/reset-password`.
- **Proactive OAuth maintenance**: tokens refresh ~5 minutes before expiry by a scheduler, reusing the same refresh machinery as manual refresh.

## Quick start (Docker)

```bash
docker run -d \
  --name vansgo \
  -p 127.0.0.1:20128:20128 \
  -v vansgo-data:/app/data \
  -e INITIAL_PASSWORD='replace-with-a-strong-password' \
  -e NODE_ENV=production \
  --restart unless-stopped \
  ghcr.io/zanonaa/vansgo:latest
```

Open `http://localhost:20128/masuk` and log in with `INITIAL_PASSWORD`. The port is loopback-only by default; for external access put an HTTPS reverse proxy with its own authentication in front.

Alternatively with Compose:

```bash
cp .env.example .env && nano .env   # set INITIAL_PASSWORD
docker compose up -d                # headroom runs inside the image; enable the toggle in the dashboard
```

Migrating **from the former VansRouter**: keep your existing data volume and either set `DATA_DIR` to the old path or attach the legacy `vansrouter-data` volume read-only at `/migration-data`. The Go startup migration imports legacy data without discarding the source (see `docs/MIGRATION.md` for the full 9Router procedure).

Building locally:

```bash
docker build -t vansgo:go-local .
```

The image builds the SPA and the Go binary in separate stages. Node stays in the runtime only for PXPIPE, which runs its package in a subprocess.

## Using the API

Create a provider connection and an API key in the dashboard, then list available IDs (providers and combos):

```bash
export VANSGO_API_KEY='your-dashboard-key'

curl --fail-with-body http://localhost:20128/v1/models \
  -H "Authorization: Bearer $VANSGO_API_KEY"
```

OpenAI-compatible chat call (replace `provider/model` with an ID from your installation):

```bash
curl --fail-with-body http://localhost:20128/v1/chat/completions \
  -H "Authorization: Bearer $VANSGO_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "provider/model",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": false
  }'
```

For SSE use `"stream": true` and `curl -N`. Other formats and services depend on the executor and the provider's capabilities; being listed in the catalog does not guarantee an implemented executor.

## Configuration

| Variable | Meaning |
| --- | --- |
| `PORT` | Go HTTP port; default `20128`. Keep the container's internal port matching the mapping. |
| `DATA_DIR` | Database and persistent data; `/app/data` in the Docker image, `~/.vansgo` for native runs. |
| `SPA_DIR` | Compiled SPA directory; `/app/spa` in the image or `frontend/dist` for local builds. |
| `INITIAL_PASSWORD` | Initial dashboard password on new installs. Set a strong value. |
| `JWT_SECRET` / `API_KEY_SECRET` | Auth secrets. When omitted, the app generates and persists them under `DATA_DIR`. Do not use known placeholders in production. |
| `HEADROOM_URL` | Legacy env. Still honored as the default `headroomUrl`; the dashboard setting wins. Embedded Headroom uses `http://localhost:8787`. |
| `VANSGO_TOOL_GUARD` | Set `off` to disable the Tool Call Guard globally. |
| `OUTBOUND_AUDIT` | Set `true` to log outbound identity attribution for every attempt (diagnostics). |
| `NODE_ENV` | Use `production` in deployments to enable production config checks. |

Provider connections, API keys, combos and Token Saver options are managed in the dashboard. Protect the data directory: it holds credentials and usage history.

## Usage & Token Savers

- **RTK** is on by default: it compresses tool results when it recognizes a known format; it does not necessarily reduce every request.
- **Headroom, Caveman, Ponytail, PXPIPE** depend on each integration's requirements; none is a marketing bullet — see `docs/official-token-savers.md`.
- To compare a request without the Token Saver pipeline, send `x-9router-token-saver: off`; the header disables the whole pipeline for that request.
- **Tool Call Guard** removes invented optional references in chat-completions tool calls. Bypass per request with `x-9router-tool-guard: off` or globally with `VANSGO_TOOL_GUARD=off`. It is not the same as validating resource existence in the client.

## Upgrading

```bash
curl --fail-with-body http://localhost:20128/api/health
curl --fail-with-body http://localhost:20128/api/version
```

`/api/version` compares your build against the GitHub Releases of this repository (`zanonaa/VansGo`). The `latest` tag is a mutable multi-arch manifest (amd64 + arm64 in one index): re-pull the image and recreate the container keeping the same volume. Restarting the container alone does not fetch a new image. Do not use `docker compose down -v` to update — it can delete volumes. Take a consistent SQLite backup before upgrading.

## Development

Requirements: **Go 1.25+**, **Node.js 22.5+**, **pnpm 9.15.9**.

```bash
pnpm install --frozen-lockfile
pnpm test
pnpm build

cd backend
go vet ./...
go test ./...
go test -race ./internal/http/handlers/...
go build -o /tmp/vansgo ./cmd/api
```

Serve the compiled SPA from Go:

```bash
cd backend
SPA_DIR="$(pwd)/../frontend/dist" \
  INITIAL_PASSWORD='replace-with-a-strong-password' \
  go run ./cmd/api
```

The server listens on `http://localhost:20128`. Outside Docker, data goes to `~/.vansgo` by default; use `DATA_DIR` to choose a different path. For frontend hot reload run `pnpm dev` in another terminal and open `http://localhost:20129` (Vite proxies `/api` to the Go server; `SPA_API_TARGET` changes that target).

### Project layout

```text
backend/                  API, gateway, authentication, SQLite and Go HTTP server
frontend/
  src/spa/                SPA entry and routes
  src/app/                UI pages
  src/shared/             Shared components, hooks and resources
  src/catalogs/           Model, capability and pricing catalogs
  src/config/             UI-side configuration
  public/                 Public assets and translations
  tests/                  JavaScript tests (includes legacy reference oracles)
  scripts/                Build and validation tooling
Dockerfile                Integrated Go + SPA build
pnpm-workspace.yaml       Frontend workspace
```

## Known limitations of the migration

The Go backend does **not** claim proven full parity with the original JavaScript backend.

- Executors not yet ported return **501**: `kiro`, `cursor`, `antigravity`, `grok-web`, `perplexity-web` and `muse-spark-web`.
- The unified reasoning-parameter transformation from the JS side is not a fully validated parity in Go yet; the test references are not runtime implementations.
- The Tool Call Guard acts on chat-completions outputs; it does not offer the same protection over native Claude/Gemini/Responses outputs.
- Live persistence of full details in `requestDetails` is not implemented yet. Do not confuse that tab with the aggregated usage history.
- Upstream limits, credits and quotas still apply, including in combos with fallback.
- Some non-chat paths (video, media copy, transcription, provider validation/discovery) still make direct outbound connections without the connection's proxy.

## License

MIT — see [LICENSE](LICENSE).

Copyright (c) 2026 zanonaa (VansGo), and Copyright (c) 2024-2026 decolua and contributors for the original VansRouter project that VansGo rewrote. Historically-committed contributions from third-party authors remain attributed in the original repository's git history.
