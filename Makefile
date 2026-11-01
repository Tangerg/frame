export GOWORK := off
.PHONY: check test-go test-js example build hooks
check: test-go test-js
test-go:
	@test -z "$$(find . \( -name node_modules -o -name .git -o -name dist \) -prune -o -name '*.go' -type f -exec gofmt -l {} +)"
	go test -race ./...
	go vet ./...
test-js:
	npm run check
example:
	npm run build
	go run ./examples/document-indexer
	go test -tags frame_interop -count=1 ./conformance
build:
	go build ./cmd/frame ./cmd/frame-runner
	npm run build

hooks:
	git config core.hooksPath .githooks
