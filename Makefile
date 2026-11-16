export GOWORK := off
.PHONY: check test-go test-js security tools example build hooks
check: test-go test-js security
tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
	go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
security:
	govulncheck ./...
test-go:
	@test -z "$$(find . \( -name node_modules -o -name .git -o -name dist \) -prune -o -name '*.go' -type f -exec gofmt -l {} +)"
	go test -race ./...
	go vet ./...
	golangci-lint run ./...
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
