#!/usr/bin/env bash
# devin-to-api setup: verify prerequisites, persist PROXY_API_KEY in .env,
# build + start via docker compose on the host IP, and walk through
# `devin auth login` (web + paste code) when no Devin session exists.
#
# Usage:
#   ./setup.sh                 # interactive — runs the login flow if needed
#   PORT=9090 ./setup.sh       # custom host port
#   BIND=127.0.0.1 ./setup.sh  # localhost only (default: 0.0.0.0 for VPS use)
set -euo pipefail

PORT="${PORT:-8080}"
BIND="${BIND:-0.0.0.0}"
HOST_CRED_DIR="${DEVIN_HOST_CRED_DIR:-$HOME/.local/share/devin}"
ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"
export PORT BIND

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- 1. prerequisites ----------
command -v docker >/dev/null 2>&1 || err "docker is not installed"
docker info >/dev/null 2>&1       || err "cannot talk to the docker daemon (permissions?)"
docker compose version >/dev/null 2>&1 || err "docker compose plugin not found"

# ---------- 2. PROXY_API_KEY — reuse from .env, else generate & persist ----------
if [ -f .env ] && grep -q '^PROXY_API_KEY=.' .env; then
  PROXY_API_KEY="$(grep -oP '^PROXY_API_KEY=\K.*' .env)"
  log "Reusing PROXY_API_KEY from .env"
else
  PROXY_API_KEY="sk-devin-$(openssl rand -hex 20 2>/dev/null \
    || od -An -tx1 -N20 /dev/urandom | tr -d ' \n')"
  printf 'PROXY_API_KEY=%s\n' "$PROXY_API_KEY" >> .env
  log "Generated PROXY_API_KEY → saved to .env"
fi
export PROXY_API_KEY

# ---------- 3. credentials dir must exist for the bind mount ----------
mkdir -p "$HOST_CRED_DIR"

# ---------- 4. build + (re)create ----------
log "Building and starting (port $BIND:$PORT)"
docker compose up -d --build --remove-orphans

# ---------- 5. devin auth login (web + paste code) ----------
authenticated() {
  curl -fs --max-time 10 "http://127.0.0.1:$PORT/healthz" 2>/dev/null \
    | grep -q '"devin_authenticated": *true'
}

sleep 2
if authenticated; then
  log "Devin session already valid (mounted credentials)"
elif [ -f "$HOST_CRED_DIR/credentials.toml" ]; then
  warn "credentials.toml exists but healthz reports unauthenticated — check it contains windsurf_api_key"
else
  log "No Devin session — starting web login."
  log "Open the printed URL in ANY browser, sign in, copy the code, paste it here."
  docker compose exec devin-to-api devin auth login --force-manual-token-flow || true
  authenticated || err "login did not complete — retry: docker compose exec devin-to-api devin auth login --force-manual-token-flow"
fi
log "Devin session verified via /healthz"

# ---------- 6. summary — always read the real key back from .env ----------
API_KEY_NOW="$(grep -oP '^PROXY_API_KEY=\K.*' .env)"
HOST_IP="$(curl -fs4 --max-time 5 https://api.ipify.org 2>/dev/null \
  || hostname -I 2>/dev/null | awk '{print $1}' \
  || echo '127.0.0.1')"
BASE="http://$HOST_IP:$PORT"

cat <<EOF

────────────────────────────────────────────────────────────
 devin-to-api is up on $BIND:$PORT

   Chat Completions : $BASE/v1/chat/completions   (apiType "chat-completions")
   Responses        : $BASE/v1/responses          (apiType "responses")
   Messages         : $BASE/v1/messages           (apiType "messages")
   Models           : $BASE/v1/models
   Health           : $BASE/healthz
   Dashboard        : $BASE/panel

   PROXY_API_KEY    : $API_KEY_NOW
   (source of truth: $ROOT/.env — clients send: Authorization: Bearer <key>)

 VS Code chatLanguageModels.json:
 {
   "name": "Devin",
   "vendor": "customendpoint",
   "apiKey": "$API_KEY_NOW",
   "apiType": "chat-completions",
   "models": [{
     "id": "swe-2-max", "name": "Devin SWE-2 Max",
     "url": "$BASE/v1/chat/completions",
     "toolCalling": true, "vision": true,
     "maxInputTokens": 128000, "maxOutputTokens": 16000
   }]
 }

 Re-login:  docker compose exec devin-to-api devin auth login --force-manual-token-flow
 Logs:      docker compose logs -f
 Stop:      docker compose down
────────────────────────────────────────────────────────────
EOF
