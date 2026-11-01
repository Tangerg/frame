#!/bin/sh
set -eu

cd "$(dirname "$0")/.."
work=$(mktemp -d "${TMPDIR:-/tmp}/frame-browser-workflows.XXXXXXXX")
trap 'rm -rf "$work"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

export GOWORK=off
go build -o "$work/authorhost" ./examples/authoring/cmd/authorhost
go build -o "$work/document" ./examples/document/cmd/document

npm run build --workspace @frame/sdk
npm run build --workspace @frame/browser
npm run build --workspace @frame/example-author-browser
npm run build --workspace @frame/example-document-web

export FRAME_AUTHOR_HOST_BINARY="$work/authorhost"
export FRAME_DOCUMENT_BINARY="$work/document"

npm run test:workflow --workspace @frame/example-author-browser
npm run test:workflow --workspace @frame/example-document-web
if [ -n "${FRAME_CHROME_EXECUTABLE:-}" ]; then
    npm run test:browser --workspace @frame/example-document-web
fi
