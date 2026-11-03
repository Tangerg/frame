//go:build flameinterop

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

// The unmodified plugin reads a document through its call grant and delivers
// Markdown through a different, acknowledged chunk grant in the same invocation.
func TestFlameTrajectoryInterop(t *testing.T) {
	var facts struct {
		Read     portable.MethodDescriptor `json:"read"`
		Document json.RawMessage           `json:"document"`
	}
	source, err := os.ReadFile(os.Getenv("FRAME_TRAJECTORY_INPUT"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(source, &facts); err != nil {
		t.Fatal(err)
	}
	read, err := portable.AdmitMethod(facts.Read)
	if err != nil {
		t.Fatal(err)
	}
	const methodName = "flame.session-markdown/render.v1"
	data, err := os.ReadFile(filepath.Join(os.Getenv("FRAME_FLAME_ROOT"), "plugins/trajectory/package/io.github.tangerg.flame/contracts/capability.generated.json"))
	if err != nil {
		t.Fatal(err)
	}
	contract, err := ParseManagedContract(data)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := NewCallbackReceiver(CallbackReceiverLimits{MaxCalls: 2, MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20, ReadTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	digest := value.Hash(facts.Document).String()
	var requests atomic.Int32
	type documentID struct {
		Digest string `json:"digest"`
		Bytes  int    `json:"bytes"`
	}
	registered, err := frame.NewPortableContract[process.Service]("1", contract.Capability())
	if err != nil {
		t.Fatal(err)
	}
	var approved frame.ExportRef
	protocol := ManagedProtocol{
		Exports: func(binding frame.UnitBinding) (map[string]ManagedExport, error) {
			unit := binding.Source
			approved = frame.ExportRef{InstallationID: unit.InstallationID, Release: unit.Release, UnitID: unit.UnitID, Export: "service", Contract: registered.Ref()}
			return map[string]ManagedExport{"service": {Name: "markdown", Contract: contract}}, nil
		},
		OpenCall: func(ctx context.Context, session *sdk.ClientSession, call ManagedCall) (*ManagedCallback, error) {
			var requested documentID
			if err := value.DecodeValue(call.Input.Bytes(), &requested, "target"); err != nil {
				return nil, err
			}
			if session == nil || call.Source != approved || call.Method.Name() != methodName || call.CommandID != "" || requested.Digest != digest || requested.Bytes != len(facts.Document) {
				return nil, frame.ErrUnavailable
			}
			grant, err := receiver.Grant(ctx, read, func(_ context.Context, id string, input value.JSON) (value.JSON, error) {
				var r struct {
					Digest string `json:"digest"`
					Offset int64  `json:"offset"`
				}
				if err := value.DecodeValue(input.Bytes(), &r, "range"); err != nil {
					return value.JSON{}, err
				}
				if id != "" || r.Digest != requested.Digest || r.Offset != 0 {
					return value.JSON{}, frame.ErrInvalid
				}
				requests.Add(1)
				return value.EncodeJSON(base64.StdEncoding.EncodeToString(facts.Document))
			})
			if err != nil {
				return nil, err
			}
			return &grant, nil
		},
		OpenStream: receiver.Stream,
	}
	checkExecution(t, protocol, contract.Capability(), func(t *testing.T, dir string) process.Program {
		name := os.Getenv("FRAME_FLAME_TRAJECTORY")
		image, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return process.Program{Command: name, ExecutableDigest: value.Hash(image), Directory: dir, Env: []string{"PLUGIN_DATA=" + dir}}
	}, nil, func(ctx context.Context, borrowed process.Service, _ string) {

		in, err := value.EncodeJSON(documentID{digest, len(facts.Document)})
		if err != nil {
			t.Fatal(err)
		}
		var markdown strings.Builder
		out, err := borrowed.(process.StreamingService).Stream(ctx, queryInvocation(t, methodName, in), func(_ context.Context, chunk value.JSON) error {
			var text string
			if err := value.DecodeValue(chunk.Bytes(), &text, "markdown"); err != nil {
				return err
			}
			_, err := markdown.WriteString(text)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		want := "# Frame integration\n"
		var id documentID
		if err := value.DecodeValue(out.Bytes(), &id, "result"); err != nil {
			t.Fatal(err)
		}
		if requests.Load() != 1 || markdown.String() != want || id != (documentID{value.Hash([]byte(want)).String(), len(want)}) {
			t.Fatalf("reads=%d markdown=%q identity=%+v", requests.Load(), markdown.String(), id)
		}
	})
}

func TestFlameManagedContractCorpus(t *testing.T) {
	raw, err := os.ReadFile(os.Getenv("FRAME_FLAME_CORPUS"))
	if err != nil {
		t.Fatal(err)
	}
	var packages []struct {
		Plugin    string
		Contracts []string
	}
	if err := json.Unmarshal(raw, &packages); err != nil || len(packages) == 0 {
		t.Fatalf("invalid build plan corpus: %v", err)
	}
	total := 0
	for _, pkg := range packages {
		t.Run(pkg.Plugin, func(t *testing.T) {
			root, err := os.OpenRoot(filepath.Join(os.Getenv("FRAME_FLAME_ROOT"), "plugins", pkg.Plugin, "package"))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			for _, path := range pkg.Contracts {
				raw, err := root.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ParseManagedContract(raw); err != nil {
					t.Fatalf("%s: %v", path, err)
				}
			}
		})
		total += len(pkg.Contracts)
	}
	t.Logf("admitted %d managed descriptors across %d plugin packages", total, len(packages))
}
