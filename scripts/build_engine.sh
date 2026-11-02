#!/usr/bin/env sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
engine_go=${PROXY_SESSION_LAB_GO:-go}
if ! command -v "$engine_go" >/dev/null 2>&1; then
  echo 'Go 1.27+ is required. Set PROXY_SESSION_LAB_GO to an installed Go executable.' >&2
  exit 2
fi
cd "$project_root/engine"
mkdir -p bin
"$engine_go" build -trimpath -o bin/proxy-lab-engine ./cmd/proxy-lab-engine
exec ./bin/proxy-lab-engine version
