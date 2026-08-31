#!/usr/bin/env bash
#
# Regenerate the web UI screenshots in docs/assets/screenshots/.
#
#   make screenshots
#
# Everything is synthetic: the script generates its own throwaway config and
# obviously-fake credential/token files in a temp directory, starts the UI
# against them, captures the shots, and deletes the directory. It never reads
# your real config.yaml, never touches secrets/, and never contacts Google —
# the `ui` subcommand performs no calendar I/O unless someone presses
# "Sync now", which this script does not do.
#
# Requires: Go, and Node (Playwright's browser is downloaded on first run).

set -euo pipefail

cd "$(dirname "$0")/../.."
REPO_ROOT="$(pwd)"

PORT="${CB_SCREENSHOT_PORT:-8791}"
OUT_DIR="${CB_SCREENSHOT_DIR:-docs/assets/screenshots}"

# A FIXED path rather than mktemp, because the file paths are visible in the
# screenshots: a temp directory would stamp each image with the capturing
# machine's own /var/folders/... or /tmp/xxxx path, which is both ugly and
# needless information about that machine. This path is stable across machines,
# so a re-run produces comparable images.
FIXTURE_DIR="${CB_FIXTURE_DIR:-/tmp/calendar-bridge-fixture}"
MARKER="$FIXTURE_DIR/.calendar-bridge-screenshot-fixture"

# Refuse to touch the directory unless it is empty or one we created, so a
# mistyped CB_FIXTURE_DIR can never delete anything of yours.
if [ -e "$FIXTURE_DIR" ] && [ ! -e "$MARKER" ]; then
  echo "refusing to use $FIXTURE_DIR: it exists and was not created by this script." >&2
  echo "Remove it yourself, or set CB_FIXTURE_DIR to a different path." >&2
  exit 1
fi

SERVER_PID=""
cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [ -e "$MARKER" ]; then
    rm -rf "$FIXTURE_DIR"
  fi
}
trap cleanup EXIT

rm -rf "$FIXTURE_DIR"
mkdir -p "$FIXTURE_DIR"
touch "$MARKER"

echo "==> building calendar-bridge"
go build -o "$FIXTURE_DIR/calendar-bridge" ./cmd/calendar-bridge

echo "==> generating a synthetic fixture in $FIXTURE_DIR"
mkdir -p "$FIXTURE_DIR/secrets"

# Fabricated OAuth client documents and tokens. There is no real Google
# project, client, or account behind any of these values.
for account in personal work-acme work-globex; do
  cat > "$FIXTURE_DIR/secrets/$account-credentials.json" <<JSON
{
  "installed": {
    "client_id": "000000000000-fixture.apps.googleusercontent.com",
    "project_id": "calendar-bridge-screenshots",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token",
    "client_secret": "NOT-A-REAL-SECRET",
    "redirect_uris": ["http://localhost"]
  }
}
JSON
  cat > "$FIXTURE_DIR/secrets/$account-token.json" <<JSON
{
  "access_token": "fake-access-token",
  "refresh_token": "fake-refresh-token",
  "token_type": "Bearer",
  "expiry": "2099-01-01T00:00:00Z"
}
JSON
  chmod 600 "$FIXTURE_DIR/secrets/$account"-*.json
done

sed -e "s|__FIXTURE_DIR__|$FIXTURE_DIR|g" \
    -e "s|127.0.0.1:8791|127.0.0.1:$PORT|g" \
    scripts/screenshots/fixture-config.yaml > "$FIXTURE_DIR/config.yaml"

echo "==> starting the UI on 127.0.0.1:$PORT"
"$FIXTURE_DIR/calendar-bridge" ui -config "$FIXTURE_DIR/config.yaml" \
  > "$FIXTURE_DIR/ui.log" 2>&1 &
SERVER_PID=$!

# Wait for the listener rather than sleeping a fixed amount.
for _ in $(seq 1 50); do
  if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/api/status" 2>/dev/null; then
    break
  fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "the UI exited before it started listening:" >&2
    cat "$FIXTURE_DIR/ui.log" >&2
    exit 1
  fi
  sleep 0.2
done

echo "==> capturing screenshots into $OUT_DIR"
mkdir -p "$OUT_DIR"
cd "$REPO_ROOT/scripts/screenshots"
npm install --no-audit --no-fund --silent
CB_UI_URL="http://127.0.0.1:$PORT" \
CB_SCREENSHOT_DIR="$REPO_ROOT/$OUT_DIR" \
  npx --yes playwright install chromium --only-shell >/dev/null 2>&1 || \
  npx --yes playwright install chromium
CB_UI_URL="http://127.0.0.1:$PORT" \
CB_SCREENSHOT_DIR="$REPO_ROOT/$OUT_DIR" \
  node capture.mjs

cd "$REPO_ROOT"
echo
echo "==> checking the captured images for anything that looks like real data"
./scripts/screenshots/check-no-real-data.sh "$OUT_DIR"

echo
echo "done. Screenshots are in $OUT_DIR/"
