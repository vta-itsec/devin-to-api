#!/usr/bin/env bash
# devin-to-api setup: verify prerequisites, build the image, authenticate
# Devin (web login + paste code), and expose the proxy on this host's IP.
#
# Usage:
#   ./setup.sh                 # interactive — runs docker login flow if needed
#   PORT=9090 ./setup.sh       # custom host port
#   PROXY_API_KEY=xxx ./setup.sh
#   BIND=127.0.0.1 ./setup.sh  # localhost only (default: 0.0.0.0 for VPS use)
set -euo pipefail

IMAGE="${IMAGE:-devin-to-api:latest}"
CONTAINER="${CONTAINER:-devin-to-api}"
PORT="${PORT:-8080}"
BIND="${BIND:-0.0.0.0}"
DATA_VOLUME="${DATA_VOLUME:-devin-to-api-data}"
HOST_CRED_DIR="${DEVIN_HOST_CRED_DIR:-$HOME/.local/share/devin}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- 1. prerequisites ----------
command -v docker >/dev/null 2>&1 || err "docker is not installed"
docker info >/dev/null 2>&1       || err "cannot talk to the docker daemon (permissions?)"

# ---------- 2. proxy API key (client → proxy auth) ----------
if [ -z "${PROXY_API_KEY:-}" ]; then
  PROXY_API_KEY="$(openssl rand -hex 24 2>/dev/null \
    || od -An -tx1 -N24 /dev/urandom | tr -d ' \n')"
  log "Generated PROXY_API_KEY (save this — clients/VS Code need it): $PROXY_API_KEY"
fi

# ---------- 3. build ----------
log "Building image $IMAGE"
docker build -t "$IMAGE" "$(cd "$(dirname "$0")" && pwd)"

# ---------- 4. (re)create container ----------
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true

# Credentials source: reuse the host's `devin auth login` if present,
# otherwise a named volume we can log into inside the container.
if [ -f "$HOST_CRED_DIR/credentials.toml" ]; then
  log "Host Devin credentials found ($HOST_CRED_DIR/credentials.toml) — bind-mounting"
  VOLUME_ARGS=(-v "$HOST_CRED_DIR:/home/devin/.local/share/devin")
else
  warn "No host credentials at $HOST_CRED_DIR/credentials.toml — using volume $DATA_VOLUME"
  VOLUME_ARGS=(-v "$DATA_VOLUME:/home/devin/.local/share/devin")
fi

docker run -d --name "$CONTAINER" \
  -p "$BIND:$PORT:8080" \
  -e PROXY_API_KEY="$PROXY_API_KEY" \
  "${VOLUME_ARGS[@]}" \
  --restart unless-stopped \
  "$IMAGE" >/dev/null
log "Container $CONTAINER listening on $BIND:$PORT"

# ---------- 5. devin auth login (web + paste code) ----------
authenticated() {
  curl -fs --max-time 10 "http://127.0.0.1:$PORT/healthz" 2>/dev/null \
    | grep -q '"devin_authenticated": *true'
}

sleep 2
if ! authenticated; then
  log "No Devin session yet — starting web login."
  log "A URL will appear; open it in ANY browser, sign in, copy the code, paste it here."
  docker exec -it "$CONTAINER" devin auth login --force-manual-token-flow
fi
authenticated || err "devin auth login did not complete — re-run: docker exec -it $CONTAINER devin auth login --force-manual-token-flow"
log "Devin session verified via /healthz"

# ---------- 6. summary ----------
HOST_IP="$(curl -fs4 --max-time 5 https://api.ipify.org 2>/dev/null \
  || hostname -I 2>/dev/null | awk '{print $1}' \
  || echo '127.0.0.1')"
BASE="http://$HOST_IP:$PORT"

cat <<EOF

────────────────────────────────────────────────────────────
 devin-to-api is up.

   Chat Completions : $BASE/v1/chat/completions   (apiType "chat completions")
   Responses        : $BASE/v1/responses          (apiType "responses")
   Messages         : $BASE/v1/messages           (apiType "messages")
   Models           : $BASE/v1/models
   Health           : $BASE/healthz
   Dashboard        : $BASE/panel

   PROXY_API_KEY    : $PROXY_API_KEY
   (clients send it as:  Authorization: Bearer \$PROXY_API_KEY)

 VS Code custom endpoint (chat.json):
 {
   "name": "Devin",
   "vendor": "customendpoint",
   "apiKey": "$PROXY_API_KEY",
   "apiType": "messages",
   "models": [{
     "id": "swe-2-max", "name": "Devin SWE-2 Max",
     "url": "$BASE/v1/messages",
     "toolCalling": true, "vision": true,
     "maxInputTokens": 128000, "maxOutputTokens": 16000
   }]
 }

 Re-login later:  docker exec -it $CONTAINER devin auth login --force-manual-token-flow
────────────────────────────────────────────────────────────
EOF
