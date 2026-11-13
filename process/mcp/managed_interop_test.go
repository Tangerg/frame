//go:build flameinterop

package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
)

// This test runs an independently built, unmodified Flame hooks backend. See MANAGED.md.
func TestFlameHooksInterop(t *testing.T) {
	binary := os.Getenv("FRAME_FLAME_HOOKS")
	descriptor, err := os.ReadFile(os.Getenv("FRAME_FLAME_HOOKS_CONTRACT"))
	if err != nil || binary == "" {
		t.Fatalf("set FRAME_FLAME_HOOKS and FRAME_FLAME_HOOKS_CONTRACT: %v", err)
	}
	contract, err := ParseManagedContract(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	profile := ManagedProtocol{Exports: func(framehost.UnitBinding) (map[string]ManagedExport, error) {
		return map[string]ManagedExport{"service": {Name: "hook-provider", Contract: contract}}, nil
	}}
	checkExecution(t, profile, contract.Capability(), func(t *testing.T, dir string) process.Program {
		image, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		return process.Program{Command: binary, ExecutableDigest: value.Hash(image), Directory: dir, Env: []string{"PLUGIN_DATA=" + dir}}
	}, nil, func(ctx context.Context, borrowed frame.Service, dir string) {
		service := borrowed.(*ManagedService)
		query, err := value.EncodeJSON(map[string]any{"cwd": dir, "projectRoot": dir, "includeProject": false})
		if err != nil {
			t.Fatal(err)
		}
		const discover = "flame.hooks/discover.v1"
		if err := service.ValidateInput(ctx, discover, query); err != nil {
			t.Fatal(err)
		}
		output, err := service.Invoke(ctx, queryInvocation(t, discover, query))
		if err != nil || output.String() != "[]" {
			t.Fatalf("discover %s, %v", output, err)
		}
		input, err := value.EncodeJSON(map[string]any{"rules": []any{}, "input": map[string]any{"event": "SessionStart", "cwd": filepath.Clean(dir)}})
		if err != nil {
			t.Fatal(err)
		}
		const evaluate = "flame.hooks/evaluate.v1"
		const id = "frame-interop-command"
		if _, err := service.Invoke(ctx, queryInvocation(t, evaluate, input)); !errors.Is(err, frame.ErrContractMismatch) {
			t.Fatalf("unkeyed command: %v", err)
		}
		if _, found, err := service.ReadReceipt(ctx, commandInvocation(t, id, evaluate, input)); err != nil || found {
			t.Fatalf("absent receipt: found=%v err=%v", found, err)
		}
		output, err = service.Invoke(ctx, commandInvocation(t, id, evaluate, input))
		if err != nil {
			t.Fatal(err)
		}
		receipt, found, err := service.ReadReceipt(ctx, commandInvocation(t, id, evaluate, input))
		if err != nil || !found || receipt != output {
			t.Fatalf("receipt %s, result %s, found=%v err=%v", receipt, output, found, err)
		}
	})
}
