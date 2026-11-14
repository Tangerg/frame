package mcp

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

type catalogExport struct {
	name       string
	capability *frame.Capability
}

func prepareCatalog(t *testing.T, ctx context.Context, exports []catalogExport, check func(context.Context, framehost.Setup) error) error {
	t.Helper()
	host, err := framehost.New(ctx, framehost.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if outcome, err := host.Close(ctx); err != nil || !outcome.Released {
			t.Errorf("catalog host cleanup: %+v %v", outcome, err)
		}
	})
	var declarations []framehost.ExportDeclaration
	for _, export := range exports {
		contract, err := framehost.NewPortableContract[frame.Service]("1", export.capability)
		if err != nil {
			t.Fatal(err)
		}
		if err := host.Register(contract); err != nil {
			t.Fatal(err)
		}
		declarations = append(declarations, framehost.ExportDeclaration{Name: export.name, Contract: contract.Ref()})
	}
	prepared := errors.New("catalog preparation observed")
	plugin, err := framehost.Define(framehost.Definition{
		Revision: value.Hash([]byte("catalog-admission")),
		Units: []framehost.Unit{{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: declarations},
			Setup: func(ctx context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
				if err := check(ctx, setup); err != nil {
					return nil, err
				}
				return nil, prepared
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Install(ctx, framehost.InstallationConfig{ID: "catalog", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	root := framehost.ExportRef{InstallationID: "catalog", Release: plugin.Revision(), UnitID: "main", Export: declarations[0].Name, Contract: declarations[0].Contract}
	_, err = host.Activate(ctx, root)
	if errors.Is(err, prepared) {
		return nil
	}
	return err
}

func connectPreparedCatalog(t *testing.T, ctx context.Context, connection process.Connection, server *sdk.Server) ([]framehost.ExportValue, error) {
	t.Helper()
	clientPipe, serverPipe := net.Pipe()
	t.Cleanup(func() { _ = clientPipe.Close(); _ = serverPipe.Close() })
	serverSession, err := server.Connect(ctx, &sdk.IOTransport{Reader: serverPipe, Writer: serverPipe}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	return connection.Connect(ctx, clientPipe)
}

func TestToolsPrepareRejectsContradictorySharedSchemas(t *testing.T) {
	for _, conflict := range []string{"input", "output"} {
		t.Run(conflict, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var exports []catalogExport
			for index, name := range []string{"first", "second"} {
				member := frame.MethodDescriptor{Name: "write", Kind: frame.Command, Input: []byte(inputSchema), Output: []byte(outputSchema)}
				if index == 1 {
					if conflict == "input" {
						member.Input = []byte(`{"type":"object","properties":{"action":{"type":"integer"}},"additionalProperties":false}`)
					} else {
						member.Output = []byte(`{"type":"boolean"}`)
					}
				}
				capability, err := frame.NewCapability("catalog."+name, member)
				if err != nil {
					t.Fatal(err)
				}
				exports = append(exports, catalogExport{name, capability})
			}
			err := prepareCatalog(t, ctx, exports, func(_ context.Context, setup framehost.Setup) error {
				connection, err := (Tools{MaxCatalogPages: 1}).Prepare(setup, nil, 1<<20)
				if connection != nil {
					_ = connection.Close(context.Background())
				}
				return err
			})
			if !errors.Is(err, frame.ErrContractMismatch) {
				t.Fatalf("contradictory catalog crossed prepare: %v", err)
			}
			for _, evidence := range []string{"write", "first", "second", conflict} {
				if !strings.Contains(err.Error(), evidence) {
					t.Fatalf("conflict diagnostic lost %q: %v", evidence, err)
				}
			}
		})
	}
}

func TestToolsPrepareRetainsSharedViewsAndRemoteRequirements(t *testing.T) {
	for _, envelopeFirst := range []bool{false, true} {
		for _, readOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("envelope-first=%t/read-only=%t", envelopeFirst, readOnly), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var exports []catalogExport
				for _, name := range []string{"structured", "envelope"} {
					member := frame.MethodDescriptor{Name: "read", Kind: frame.Query, Input: []byte(inputSchema), Output: []byte(outputSchema)}
					if name == "envelope" {
						member.Kind = frame.Command
						member.Input = []byte(`{"properties":{"action":{"type":"string"}},"additionalProperties":false,"type":"object"}`)
						member.Output = []byte(`{"type":"object","required":["content"],"properties":{"content":{"type":"array"}}}`)
					}
					capability, err := frame.NewCapability("catalog."+name, member)
					if err != nil {
						t.Fatal(err)
					}
					exports = append(exports, catalogExport{name, capability})
				}
				if envelopeFirst {
					slices.Reverse(exports)
				}
				protocol := Tools{MaxCatalogPages: 1, FullResult: func(export framehost.ExportDeclaration, _ string) bool { return export.Name == "envelope" }}
				err := prepareCatalog(t, ctx, exports, func(ctx context.Context, setup framehost.Setup) error {
					connection, err := protocol.Prepare(setup, nil, 1<<20)
					if err != nil {
						return err
					}
					server := sdk.NewServer(&sdk.Implementation{Name: "shared-tools", Version: "1"}, nil)
					server.AddTool(&sdk.Tool{Name: "read", InputSchema: json.RawMessage(inputSchema), OutputSchema: json.RawMessage(outputSchema), Annotations: &sdk.ToolAnnotations{ReadOnlyHint: readOnly}},
						func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
							return &sdk.CallToolResult{StructuredContent: json.RawMessage(`{"n":1}`), Content: []sdk.Content{&sdk.TextContent{Text: "shared result"}}}, nil
						})
					values, err := connectPreparedCatalog(t, ctx, connection, server)
					if err != nil {
						return err
					}
					input, err := value.ParseJSON([]byte(`{}`))
					if err != nil {
						return err
					}
					for _, exported := range values {
						invocation := queryInvocation(t, "read", input)
						if exported.Name == "envelope" {
							invocation = commandInvocation(t, "original-command", "read", input)
						}
						output, err := exported.Value.(frame.Service).Invoke(ctx, invocation)
						if err != nil {
							return err
						}
						if exported.Name == "structured" && output.String() != `{"n":1}` || exported.Name == "envelope" && !strings.Contains(output.String(), "shared result") {
							return fmt.Errorf("shared view %s changed its output: %s", exported.Name, output)
						}
					}
					return nil
				})
				if readOnly && err != nil || !readOnly && !errors.Is(err, frame.ErrContractMismatch) {
					t.Fatalf("shared catalog or remote read-only requirement changed: %v", err)
				}
			})
		}
	}
}

func TestManagedPrepareOwnsGlobalMethodIdentity(t *testing.T) {
	for _, change := range []string{"none", "input", "output", "kind", "target", "remote"} {
		t.Run(change, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			declared := make(map[string]ManagedExport)
			var exports []catalogExport
			description := managedDescription{Protocol: managedVersion}
			for index, name := range []string{"first", "second"} {
				member := frame.MethodDescriptor{Name: "write", Kind: frame.Command, Target: "document", Input: []byte(inputSchema), Output: []byte(outputSchema)}
				if index == 1 {
					member.Description = "A different presentation of the shared operation."
					switch change {
					case "input":
						member.Input = []byte(`{"type":"object","properties":{"action":{"type":"integer"}},"additionalProperties":false}`)
					case "output":
						member.Output = []byte(`{"type":"boolean"}`)
					case "kind":
						member.Kind = frame.Query
					case "target":
						member.Target = "workspace"
					}
				}
				descriptor, err := value.EncodeJSON(managedDescriptor{managedVersion, "catalog." + name, managedSelection, "installation", []frame.MethodDescriptor{member}})
				if err != nil {
					t.Fatal(err)
				}
				contract, err := ParseManagedContract(descriptor.Bytes())
				if err != nil {
					t.Fatal(err)
				}
				declared[name] = ManagedExport{Name: name, Contract: contract}
				exports = append(exports, catalogExport{name, contract.Capability()})
				description.Capabilities = append(description.Capabilities, struct {
					Name     string         `json:"name"`
					Contract jsontext.Value `json:"contract"`
				}{name, descriptor.Bytes()})
				if index == 0 {
					description.Methods = []frame.MethodDescriptor{member}
				}
			}
			if change == "remote" {
				description.Methods[0].Target = "workspace"
			}
			protocol := ManagedProtocol{Exports: func(framehost.UnitBinding) (map[string]ManagedExport, error) { return declared, nil }}
			connected := false
			err := prepareCatalog(t, ctx, exports, func(ctx context.Context, setup framehost.Setup) error {
				connection, err := protocol.Prepare(setup, nil, 1<<20)
				if err != nil {
					return err
				}
				connected = true
				server := sdk.NewServer(&sdk.Implementation{Name: "shared-managed", Version: "1"}, nil)
				for _, name := range []string{managedDescribe, managedSetup, managedActivate} {
					if err := sdk.AddReceivingCustomMethod(server, name, func(context.Context, *sdk.ServerSession, *managedParams) (*managedResult, error) {
						if name != managedDescribe {
							return &managedResult{Value: jsontext.Value(`true`)}, nil
						}
						raw, err := value.EncodeJSON(description)
						return &managedResult{Value: raw.Bytes()}, err
					}); err != nil {
						return err
					}
				}
				values, err := connectPreparedCatalog(t, ctx, connection, server)
				if err == nil && len(values) != len(exports) {
					return fmt.Errorf("shared managed exports changed: %d", len(values))
				}
				return err
			})
			switch change {
			case "none":
				if err != nil || !connected {
					t.Fatalf("legal shared methods were refused: %v", err)
				}
			case "remote":
				if !errors.Is(err, frame.ErrContractMismatch) || !connected {
					t.Fatalf("remote drift escaped connect verification: %v", err)
				}
			default:
				if !errors.Is(err, frame.ErrContractMismatch) || connected {
					t.Fatalf("contradictory method reached connect: connected=%t error=%v", connected, err)
				}
				for _, evidence := range []string{"write", "first", "second"} {
					if !strings.Contains(err.Error(), evidence) {
						t.Fatalf("conflict diagnostic lost %q: %v", evidence, err)
					}
				}
			}
		})
	}
}
