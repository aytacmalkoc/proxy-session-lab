#!/usr/bin/env bash
set -euo pipefail
APP_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
if [[ ! -x "$APP_DIR/.venv/bin/python" ]]; then
  echo "Standalone virtual environment not found: $APP_DIR/.venv" >&2
  exit 2
fi
if [[ -d "$APP_DIR/.browsers" && -z "${PLAYWRIGHT_BROWSERS_PATH:-}" ]]; then
  export PLAYWRIGHT_BROWSERS_PATH="$APP_DIR/.browsers"
fi
exec "$APP_DIR/.venv/bin/python" "$APP_DIR/app.py" "$@"
