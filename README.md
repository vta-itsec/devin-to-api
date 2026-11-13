# devin-to-api

Host **Devin CLI** as an OpenAI/Anthropic-compatible HTTP API — usable as a
VS Code *custom endpoint*, or from any client that speaks **Chat Completions**,
**Responses**, or **Messages**.

The proxy authenticates upstream with the same session token that
[`devin auth login`](https://devin.ai) stores on your machine — no manual token
extraction needed. Log in once, the proxy picks the credentials up live.

Adapted from [`leookun/devin-2api`](https://github.com/leookun/devin-2api)
(MIT) — only the source code is reused; see [Differences](#differences-vs-devin-2api).

## Endpoints

| Endpoint | Protocol | VS Code `apiType` |
| --- | --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions | `"chat-completions"` |
| `POST /v1/responses` | OpenAI Responses (+ WebSocket on `GET`) | `"responses"` |
| `POST /v1/messages` | Anthropic Messages | `"messages"` |
| `POST /v1/sessions` | Devin Cloud Sessions REST (passthrough → `api.devin.ai`) | — |
| `GET /v1/sessions` `GET /v1/sessions/{id}` `POST /v1/sessions/{id}/message` | Same passthrough | — |
| `GET /v1/models` `GET /v1/models/{id}` | OpenAI model list | — |
| `GET /healthz` | Liveness + Devin auth status | — |
| `GET /panel` | Optional dashboard | — |

All three chat protocols support streaming (SSE) and non-streaming, tool
calling, and vision-capable models.

### Devin Cloud sessions passthrough

`/v1/sessions*` forwards requests verbatim to `api.devin.ai` — body fields
are untouched, so `prompt`, `snapshot_id`, `playbook_id`, `tags`,
`unlisted`, `title` and future fields all work exactly as documented in
Devin's API. Client auth stays `PROXY_API_KEY`; upstream auth is
`DEVIN_API_KEY` (or `devin_cloud.api_key`). If no server key is set, callers
can supply their own key per request via `X-Devin-Api-Key` (works even when
`PROXY_API_KEY` occupies `Authorization`); with no header the client's
`Authorization` is forwarded. A default `snapshot_id` can be injected on
session creation via `devin_cloud.snapshot_id` /
`DEVIN_CLOUD_SNAPSHOT_ID` (explicit `snapshot_id` in the request always
wins). These are **async cloud Devins**, not chat replies — poll
`GET /v1/sessions/{id}` for status.

### `devin-cloud` chat model (cloud Devin as a VS Code model)

When `devin_cloud` is configured, any chat request whose model is
`devin-cloud` is routed to a dedicated adapter that maps the conversation to
a **Devin Cloud session**:

- first turn → `POST api.devin.ai/v1/sessions` (prompt = transcript,
  `snapshot_id` from `devin_cloud.snapshot_id` or a per-request override via
  model id `devin-cloud@snapshot-xxx`)
- next turns → `POST /v1/sessions/{id}/message` with just the new turn
- the adapter polls `GET /v1/sessions/{id}` until `finished`/`blocked` and
  returns the session output as the chat reply; progress shows up as
  thinking deltas while streaming (cloud turns take minutes — use
  streaming)

Conversation→session mapping persists in
`<workspace>/.devin-to-api-cloud-sessions.json` (same chain-matching idea as
the acp backend; identical openings get separate sessions). Cloud sessions
are autonomous — they consume ACU and run unattended, so use deliberately.

VS Code entry — just add another model under the same custom endpoint:

```json
{
    "id": "devin-cloud",
    "name": "Devin Cloud Session",
    "url": "http://<host>:8080/v1/chat/completions",
    "toolCalling": false,
    "vision": false
}
```

## Backends: `connect` vs `acp` (session mode)

The proxy can drive Devin two ways — pick with `devin.backend` /
`DEVIN_BACKEND`:

| | `connect` (stateless) | `acp` (**session mode, Docker default**) |
| --- | --- | --- |
| Mechanism | Direct `GetChatMessage` Connect RPC | Resident `devin acp` subprocess per session (Agent Client Protocol), sessions via `session/new` + `session/load` |
| Context | Each request is a brand-new trajectory; multi-turn works only because the client replays full history | **One Devin CLI session per conversation** — Devin keeps its own transcript server-side; only the *new* turn is sent |
| Tools | Client-declared tools forwarded to the model | Devin's own tools (fs/exec) run inside `workspace` — it can read your code to answer |
| Needs | `credentials.toml` token | `credentials.toml` token **+ `devin` binary** |

**How conversations map to sessions (acp):** the proxy hashes each *user*
message of the incoming history into a chain and matches the longest strict
prefix against registered sessions — assistant messages are excluded from the
chain because clients may reformat them (thinking blocks, reordering).

- **Same chat, next turn** → prefix extends → `session/load` resumes the same
  `devin` session; only the new turn is prompted.
- **New chat / edited history / fork** → the prefix diverges → a fresh `devin`
  session is created and the history is injected as a transcript prompt —
  the original session's transcript is never polluted.
- **Session tags**: every reply carries an invisible fingerprint — a short
  alias base4-encoded into zero-width characters (`U+2060<zw>U+2060`), so it
  is invisible in rendered chat *and* raw text. Clients that echo it back
  verbatim (VS Code does) get *deterministic* session routing — two chats
  with identical openings, identical replies, or interleaved turns always
  land on their own session. Tags are only read from *assistant* messages —
  a tag quoted inside a user message can never hijack a session. The older
  `<!--d2a:<session>-->` HTML-comment format is still recognized in
  pre-upgrade histories.
- **Client-side compaction / truncation**: when the client rewrites old
  history (summarized first message, dropped turns), the user-chain diverges
  at position 0. If an echoed reply still carries the session tag, the same
  `devin` session is resumed — its real transcript was never compacted — and
  only the turns after the last echoed reply are prompted. Without a tag the
  divergence is ambiguous (compaction vs. edited first message look alike),
  so the safe fork path applies: new session + transcript seed.
- **Reply fingerprints**: even without the tag, the proxy matches your last
  assistant message against stored reply hashes to disambiguate identical
  openings; ties resolve to the most recently active session.
- **Devin-side compaction** is transparent: `devin` summarizes long sessions
  internally (marked via `summarized_from` in `cli/sessions.db`), the session
  id never changes, and `session/load` keeps working — routing is unaffected.
- **Concurrency**: turns of the same conversation are serialized per session;
  a duplicate request that arrives while its twin is in flight waits, then
  replays the completed reply instead of double-prompting.
- **`conversation_id`**: clients may send `conversation_id` (chat),
  `metadata.conversation_id` (messages), or `conversation.id`/`conversation`
  (responses) for an explicit 1:1 session binding — no history replay needed,
  each request may carry only the new turn.
- **Replay**: resending an already-completed chain returns the cached reply.
- **Map persistence**: `<workspace>/.devin-to-api-sessions.json` survives
  restarts; if it is lost, the proxy recovers by matching the first user
  message against `session/list` titles (only when exactly one session
  matches) — otherwise it falls back to a transcript-seeded new session.
- All Devin sessions also appear in `devin list` on the host.

**ACP workspace:** `devin.workspace` (`DEVIN_WORKSPACE`, Docker default
`/home/devin/workspace`) is the session cwd *and* the only directory Devin's
file tools may touch. Mount a project into it (`./workspace` is already
bind-mounted in `docker-compose.yml`) to chat about real code.
`devin.acp_mode` (`bypass` by default) controls Devin's permission mode.

**Speed:** each Devin session keeps a *resident* `devin acp` process —
`initialize` + `session/load` (which replays the whole transcript) happen
only on the first turn of a session; later turns go straight to
`session/prompt`. Idle processes are reaped after `devin.acp_proc_idle`
(`DEVIN_ACP_PROC_IDLE`, default `15m`), the pool is capped at
`devin.acp_proc_max` (`DEVIN_ACP_PROC_MAX`, default `8`) with LRU eviction,
and up to 2 spare processes are pre-warmed (spawn + handshake done) so even
brand-new chats skip the cold start.

## How authentication works

`devin auth login` writes `~/.local/share/devin/credentials.toml`:

```toml
windsurf_api_key = "devin-session-token$..."
api_server_url   = "https://server.codeium.com"
```

devin-to-api reads `windsurf_api_key` from that file **on every request**, so
`devin auth login` / `devin auth logout` take effect without a restart. Check
`GET /healthz` → `devin_authenticated` to verify.

Token resolution order:

1. `devin.token` in `config.yaml`, or `DEVIN_SESSION_TOKEN` env var (static)
2. `~/.local/share/devin/credentials.toml` (`devin auth login`) — **default**
3. `devin.credentials_file` / `DEVIN_CREDENTIALS_FILE` to point elsewhere

### Which Devin credential should you use?

Devin offers three credential types — for this proxy the **`devin auth login`
session token is the right (and only viable) choice**:

| Type | Purpose | Works here? |
| --- | --- | --- |
| `devin auth login` session token (`devin-session-token$...`) | Authenticates the Devin CLI/IDE Connect API (`server.codeium.com`) — streaming LLM chat | ✅ yes |
| Devin API key (`api.devin.ai`) | REST API that creates **async Devin Cloud sessions** (autonomous agents), not chat completions | ✅ only via `/v1/sessions*` passthrough — not for chat |
| PAT / service user (`devin_pat$...`, org service accounts) | Same `api.devin.ai` sessions REST surface, org-scoped | ✅ same passthrough, org-scoped |

The `POST /v1/sessions` REST API does not return a chat completion — it spins
up a long-running cloud Devin. A chat proxy needs the Connect
`GetChatMessage` RPC, which only the session token can call. Both surfaces
are exposed side-by-side: chat endpoints use the session token, sessions
endpoints use the API key.

## Quick start

### Bare metal

```bash
devin auth login                     # once — credentials land in ~/.local/share/devin
cp config.example.yaml config.yaml   # optional; env vars alone also work
go run ./cmd/devin-to-api -config config.yaml
```

Verify:

```bash
curl http://localhost:8080/healthz
# {"devin_auth_source":"...credentials.toml","devin_authenticated":true,"status":"ok"}

curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"swe-2-max","messages":[{"role":"user","content":"hi"}]}'
```

### Docker

```bash
docker build -t devin-to-api .
# already logged in on the host? share the credentials dir:
docker run -d --name devin-to-api -p 8080:8080 \
  -v ~/.local/share/devin:/home/devin/.local/share/devin \
  -e PROXY_API_KEY=change-me \
  devin-to-api
```

`docker compose up -d` does the same via `docker-compose.yml` — or use
[`setup.sh`](setup.sh), which does everything in one shot: checks Docker,
builds the image, generates `PROXY_API_KEY`, starts the container on
`0.0.0.0:8080` (VPS-friendly), and walks you through the web login +
paste-code flow if no Devin session exists yet:

```bash
./setup.sh            # PORT=9090 BIND=127.0.0.1 PROXY_API_KEY=xxx ./setup.sh
```

#### Logging in inside Docker

The image ships the Devin CLI itself, so credentials can come from three
places — pick whichever fits:

**Option 1 — mount host credentials (most convenient).**
Run `devin auth login` on the host once, then bind-mount
`~/.local/share/devin` as shown above. The proxy re-reads
`credentials.toml` on every request, so later `devin auth login` /
`logout` on the host takes effect with no container restart.

**Option 2 — web login inside the container (no host CLI needed).**
The default browser flow cannot work in a container: it spins up a
`localhost` callback listener that a browser on your workstation can never
reach. Use the manual-token flow instead — it's the headless variant the
CLI offers for SSH sessions:

```bash
docker exec -it devin-to-api devin auth login --force-manual-token-flow
```

It prints a `https://app.devin.ai/auth/cli/continue?...` URL — open it in
any browser, sign in, copy the code shown, and paste it back.
`credentials.toml` is written inside the container (persist it with a
volume, e.g. `-v devin-data:/home/devin/.local/share/devin`) and the proxy
starts serving immediately.

**Option 3 — pass a token directly.**
Copy `windsurf_api_key` out of any existing `credentials.toml` (host,
another container, CI secret store) and inject it — no file needed:

```bash
docker run -d -p 8080:8080 \
  -e DEVIN_SESSION_TOKEN='devin-session-token$...' \
  -e PROXY_API_KEY=change-me \
  devin-to-api
```

> **Note:** `docker exec -it ... devin auth login` *without*
> `--force-manual-token-flow` prints a browser URL whose `localhost`
> redirect lands inside the container — sign-in never completes. Always
> use the manual flow flag in Docker/SSH contexts.

## VS Code custom endpoint

**Full step-by-step guide (file location, `${input:...}` secrets, mixing
apiTypes, troubleshooting): [docs/vscode-setup.md](docs/vscode-setup.md)**

`PROXY_API_KEY` protects `/v1/*`; VS Code sends it as
`Authorization: Bearer <key>` via the `apiKey` field. Pick **one** `apiType`
per model entry — all three are supported:

```json
{
    "name": "Devin",
    "vendor": "customendpoint",
    "apiKey": "${input:chat.lm.secret.-7d61d17c}",
    "apiType": "messages",
    "models": [
        {
            "id": "swe-2-max",
            "name": "Devin SWE-2 Max",
            "url": "http://localhost:8080/v1/messages",
            "toolCalling": true,
            "vision": true,
            "maxInputTokens": 128000,
            "maxOutputTokens": 16000
        }
    ]
}
```

Swap `"apiType"`/`"url"` for the other protocols:

- `"chat-completions"` → `"url": "http://localhost:8080/v1/chat/completions"`
- `"responses"` → `"url": "http://localhost:8080/v1/responses"`
- `"messages"` → `"url": "http://localhost:8080/v1/messages"`

Model IDs come from `GET /v1/models` (or `devin models list`), e.g.
`swe-2-max`, `adaptive`, `claude-opus-5-5-max`, `gpt-6-astra-max`. Leave
`"id"` empty to fall back to the server-side `devin.model` default
(`adaptive`).

## Configuration

Config is a YAML file (`-config` flag, default `config.yaml`) **plus**
environment variables — env always wins. A missing config file is fine:
env-only works, which is how the Docker image runs.

| YAML field | Env var | Default | Description |
| --- | --- | --- | --- |
| `server.listen` | `DEVIN_TO_API_LISTEN` | `:8080` | HTTP listen address |
| `server.max_concurrency` | — | `1024` | Concurrent `/v1/*` requests cap |
| `devin.backend` | `DEVIN_BACKEND` | `connect` (`acp` in docker-compose) | `connect` = stateless RPC; `acp` = persistent devin CLI sessions |
| `devin.acp_bin` | `DEVIN_ACP_BIN` | `devin` | devin executable for the acp backend |
| `devin.workspace` | `DEVIN_WORKSPACE` | `~/.devin-to-api/workspace` | acp session cwd + fs tool root |
| `devin.acp_mode` | `DEVIN_ACP_MODE` | `bypass` | Devin session permission mode |
| `devin.acp_state_file` | — | `<workspace>/.devin-to-api-sessions.json` | Conversation→session map file |
| `devin.base_url` | `DEVIN_BASE_URL` | `credentials.toml` → `https://server.codeium.com` | Connect API base URL |
| `devin.token` | `DEVIN_SESSION_TOKEN` | — | Static session token (skips credentials file) |
| `devin.credentials_file` | `DEVIN_CREDENTIALS_FILE` | `~/.local/share/devin/credentials.toml` | `devin auth login` file, re-read per request |
| `devin.model` | `DEVIN_MODEL` | `adaptive` | Default model UID when requests omit `model` |
| `devin.proxy` | `DEVIN_PROXY` | — | Upstream HTTP/SOCKS5 proxy |
| `devin.force_http1` | — | `true` | Force HTTP/1.1 upstream (avoids HTTP/2 stream serialization) |
| `auth.api_key` | `PROXY_API_KEY` | — | Key clients must send to `/v1/*` (empty = open) |
| `dashboard.password` | `DASHBOARD_PASSWORD` | — | `/panel` password (empty = open) |
| `debug.enabled` | `DEBUG` | `false` | Per-request debug logs under `logs/` |

```yaml
server:
  listen: ":8080"
devin:
  # nothing needed when logged in via `devin auth login`
  model: "adaptive"
auth:
  api_key: "change-me"
```

Notes:

- Tokens are never written to logs (redacted as `<redacted>`).
- If no token is resolvable, `/v1/*` returns `401`/`503` with a hint to run
  `devin auth login`; `/healthz` reports `devin_authenticated: false`.

## Regenerating the protobuf bindings

`devinproto/` contains generated Connect/Go bindings for the Devin
`ApiServerService`. The source proto lives in `proto/all-protos.proto`. To
regenerate after extracting new descriptors from a Devin/Windsurf binary
(`go run ./cmd/protoextract <binary> proto/`):

```bash
protoc -I proto \
  --go_out=devinproto --go_opt=module=local/devinproto --go_opt=Mall-protos.proto=local/devinproto \
  --connect-go_out=devinproto --connect-go_opt=module=local/devinproto --connect-go_opt=Mall-protos.proto=local/devinproto \
  proto/all-protos.proto
```

## Differences vs devin-2api

- **`devin auth login` integration**: reads `credentials.toml` directly —
  no sqlite extraction, no manual token copy. Re-read per request, and
  `base_url` falls back to the file's `api_server_url`.
- **Auth status reporting**: `/healthz` exposes `devin_authenticated` +
  `devin_auth_source`; startup logs the active source.
- **Env-var configuration**: every field has an env override; config file is
  optional — the Docker image runs config-free.
- **Devin CLI inside the image**: `docker exec -it <c> devin auth login`.
- Generated protobuf bindings are committed under `devinproto/` so the build
  needs only Go.

## License

MIT — adapted from [leookun/devin-2api](https://github.com/leookun/devin-2api).
See [LICENSE](LICENSE).
