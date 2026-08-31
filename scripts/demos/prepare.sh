#!/usr/bin/env bash
#
# Builds the binary and a synthetic fixture for the VHS demos.
# Called by each .tape via its Setup block.

set -euo pipefail
cd "$(dirname "$0")/../.."

DEMO_DIR="${CB_DEMO_DIR:-/tmp/calendar-bridge-demo}"
MARKER="$DEMO_DIR/.calendar-bridge-demo-fixture"

if [ -e "$DEMO_DIR" ] && [ ! -e "$MARKER" ]; then
  echo "refusing to use $DEMO_DIR: it exists and was not created by this script." >&2
  exit 1
fi

rm -rf "$DEMO_DIR"
mkdir -p "$DEMO_DIR/secrets"
touch "$MARKER"

go build -o "$DEMO_DIR/calendar-bridge" ./cmd/calendar-bridge

for account in personal work-acme; do
  cat > "$DEMO_DIR/secrets/$account-credentials.json" <<JSON
{
  "installed": {
    "client_id": "000000000000-demofixture.apps.googleusercontent.com",
    "project_id": "calendar-bridge-demo",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token",
    "client_secret": "NOT-A-REAL-SECRET",
    "redirect_uris": ["http://localhost"]
  }
}
JSON
  chmod 600 "$DEMO_DIR/secrets/$account-credentials.json"
done

cat > "$DEMO_DIR/config.yaml" <<YAML
accounts:
  - name: personal
    credentials_file: $DEMO_DIR/secrets/personal-credentials.json
    token_file: $DEMO_DIR/secrets/personal-token.json
    calendar_id: primary
  - name: work-acme
    credentials_file: $DEMO_DIR/secrets/work-acme-credentials.json
    token_file: $DEMO_DIR/secrets/work-acme-token.json
    calendar_id: primary

poll_interval: 5m
lookahead_days: 30
block_title: "Busy (calendar-bridge)"
YAML
