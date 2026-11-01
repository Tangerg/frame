#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 path/to/flame-checkout" >&2
    exit 2
fi
flame=$(cd "$1" && pwd)
frame=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/frame-flame.XXXXXXXX")
trap 'rm -rf "$work"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
fixtures="$frame/process/mcp/testdata"

(
    cd "$flame"
    export GOWORK="$flame/go.work"
    go build -o "$work/hooks" ./plugins/hooks/backend
    go build -o "$work/trajectory" ./plugins/trajectory/backend
    go build -o "$work/stream" "$fixtures/qimu-stream/main.go"
    go run "$fixtures/trajectory/main.go" > "$work/document.json"
)

cd "$frame"
export FRAME_FLAME_ROOT="$flame"
export FRAME_FLAME_HOOKS="$work/hooks"
export FRAME_FLAME_HOOKS_CONTRACT="$flame/plugins/hooks/package/io.github.tangerg.flame/contracts/capability.generated.json"
export FRAME_QIMU_STREAM="$work/stream"
export FRAME_QIMU_VECTORS="$flame/qimu/node/src/protocol.generated.json"
export FRAME_FLAME_TRAJECTORY="$work/trajectory"
export FRAME_TRAJECTORY_INPUT="$work/document.json"
GOWORK=off go test -race -tags flameinterop -count=1 -v ./process/mcp
