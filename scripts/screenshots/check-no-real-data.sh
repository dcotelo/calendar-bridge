#!/usr/bin/env bash
#
# A guard, not a guarantee: greps the OCR-free metadata and the fixture that
# produced the screenshots for anything that looks like real data, so an
# accidental capture against a real config is caught before it is committed.
#
# The real check is a human opening the images. This catches the obvious cases.

set -euo pipefail
DIR="${1:-docs/assets/screenshots}"

if [ ! -d "$DIR" ]; then
  echo "no screenshot directory at $DIR" >&2
  exit 1
fi

fail=0

# 1. Every credential and token path in the fixture must be rooted at the
#    generated temp directory. A relative "secrets/..." path would resolve
#    against the repo and could pick up the maintainer's real files.
if grep -nE '^\s*(credentials_file|token_file):' scripts/screenshots/fixture-config.yaml \
   | grep -v '__FIXTURE_DIR__' >/dev/null 2>&1; then
  echo "FAIL: fixture-config.yaml has a credential path that is not rooted at __FIXTURE_DIR__:" >&2
  grep -nE '^\s*(credentials_file|token_file):' scripts/screenshots/fixture-config.yaml \
    | grep -v '__FIXTURE_DIR__' >&2
  fail=1
fi

# 2. No screenshot should be enormous — a full-page capture of a real calendar
#    would be, and it also keeps the README's weight sane.
while IFS= read -r -d '' f; do
  size=$(wc -c < "$f")
  if [ "$size" -gt 1500000 ]; then
    echo "WARN: $f is $((size / 1024)) KB; consider re-capturing or compressing" >&2
  fi
done < <(find "$DIR" -name '*.png' -print0)

# 3. PNG metadata must not carry a filesystem path from the capturing machine.
if command -v strings >/dev/null 2>&1; then
  while IFS= read -r -d '' f; do
    if strings "$f" | grep -qE '/(Users|home)/[a-z]' ; then
      echo "FAIL: $f embeds a home-directory path in its metadata" >&2
      fail=1
    fi
  done < <(find "$DIR" -name '*.png' -print0)
fi

count=$(find "$DIR" -name '*.png' | wc -l | tr -d ' ')
if [ "$count" -eq 0 ]; then
  echo "FAIL: no screenshots found in $DIR" >&2
  exit 1
fi

if [ "$fail" -eq 0 ]; then
  echo "ok: $count screenshots, no obvious real data"
  echo "    Open them yourself before committing — this check is a guard, not a guarantee."
else
  exit 1
fi
