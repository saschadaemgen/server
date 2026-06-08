#!/usr/bin/env bash
# Pre-Push-Grep — scan the TRACKED cockpit for known sensitive values before a
# push. The real patterns live in the gitignored scripts/secret-patterns.txt
# (local only), so this tracked script itself carries NO secrets.
#
#   bash tools/cockpit/scripts/pre-push-grep.sh            # scans tools/cockpit
#   bash tools/cockpit/scripts/pre-push-grep.sh ../..      # scans the whole repo
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)" # tools/cockpit
patterns="$here/scripts/secret-patterns.txt"
target="${1:-$here}"

if [[ ! -f "$patterns" ]]; then
  echo "✗ $patterns fehlt (gitignored, lokal anzulegen mit den echten Werten)." >&2
  exit 2
fi

hits=$(grep -rnEf "$patterns" "$target" \
  --include='*.js' --include='*.go' --include='*.css' --include='*.html' \
  --include='*.md' --include='*.json' --include='*.mjs' 2>/dev/null \
  | grep -v '/node_modules/' || true)

if [[ -n "$hits" ]]; then
  echo "✗ SECRET-TREFFER in getrackten Dateien — NICHT PUSHEN:" >&2
  echo "$hits" >&2
  exit 1
fi
echo "✓ Pre-Push-Grep sauber ($target)"
