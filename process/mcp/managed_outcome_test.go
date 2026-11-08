package mcp

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

func managedServiceOverPipe(t *testing.T, ctx context.Context, method *portable.Method, invoke func(context.Context, *managedParams) (*managedResult, error), middleware ...sdk.Middleware) *ManagedService {
	t.Helper()
	descriptor, err := value.EncodeJSON(managedDescriptor{managedVersion, "outcome.v1", managedSelection, "installation", []portable.MethodDescriptor{method.Descriptor()}})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := ParseManagedContract(descriptor.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "managed-outcome", Version: "1"}, nil)
	if err := sdk.AddReceivingCustomMethod(server, managedInvoke, func(ctx context.Context, _ *sdk.ServerSession, params *managedParams) (*managedResult, error) {
		return invoke(ctx, params)
	}); err != nil {
		t.Fatal(err)
	}
	clientPipe, serverPipe := net.Pipe()
	t.Cleanup(func() { _ = clientPipe.Close(); _ = serverPipe.Close() })
	serverSession, err := server.Connect(ctx, &sdk.IOTransport{Reader: serverPipe, Writer: serverPipe}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := connect(ctx, clientPipe, 1<<20, func(client *sdk.Client) error {
		client.AddSendingMiddleware(middleware...)
		return registerManaged(client)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	lifetime := frame.NewLifetime(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if outcome, err := lifetime.Close(ctx); err != nil || !outcome.Released {
			t.Errorf("managed service cleanup: %+v %v", outcome, err)
		}
	})
	return &ManagedService{session: session, physical: "writer", contract: contract, lifetime: lifetime}
}

func TestManagedCancellationPreservesDispatchBoundary(t *testing.T) {
	for _, kind := range []portable.MethodKind{portable.Query, portable.Command} {
		for _, before := range []bool{true, false} {
			for _, wrapped := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/before=%t/wrapped=%t", kind, before, wrapped), func(t *testing.T) {
					ctx, timeout := context.WithTimeout(t.Context(), 5*time.Second)
					defer timeout()
					method, err := portable.NewMethod[string, string](portable.MethodConfig{Name: "write", Kind: kind, Target: "document"})
					if err != nil {
						t.Fatal(err)
					}
					entered := make(chan *managedParams, 1)
					var calls atomic.Int32
					service := managedServiceOverPipe(t, ctx, method, func(ctx context.Context, params *managedParams) (*managedResult, error) {
						calls.Add(1)
						entered <- params
						<-ctx.Done()
						return nil, ctx.Err()
					})
					local := &Refusal{Code: InvocationConflictCode, Message: "caller stopped waiting"}
					var cause error = local
					if wrapped {
						cause = fmt.Errorf("local operation: %w", cause)
					}
					call, cancel := context.WithCancelCause(ctx)
					defer cancel(nil)
					input, err := value.EncodeJSON("original input")
					if err != nil {
						t.Fatal(err)
					}
					invocation := queryInvocation(t, method.Name(), input)
					if kind == portable.Command {
						invocation = commandInvocation(t, "original-command", method.Name(), input)
					}
					if before {
						cancel(cause)
					}
					done := make(chan error, 1)
					go func() { _, err := service.Invoke(call, invocation); done <- err }()
					if !before {
						select {
						case params := <-entered:
							if params.CommandID != invocation.CommandID() {
								t.Fatal("SDK request replaced the original command identity")
							}
							cancel(cause)
						case <-ctx.Done():
							t.Fatal(ctx.Err())
						}
					}
					select {
					case err := <-done:
						if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) || !errors.Is(err, local) {
							t.Fatalf("local cancellation lost its cause: %v", err)
						}
						if unknown := errors.Is(err, process.ErrOutcomeUnknown); unknown == before {
							t.Fatalf("local refusal changed dispatch evidence: before=%t unknown=%t error=%v", before, unknown, err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if (calls.Load() == 0) != before {
						t.Fatalf("unexpected SDK dispatch count: before=%t calls=%d", before, calls.Load())
					}
				})
			}
		}
	}
}

func TestManagedValidatedReplySurvivesCancellationAfterReceipt(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprintf("refused=%t", refused), func(t *testing.T) {
			ctx, timeout := context.WithTimeout(t.Context(), 5*time.Second)
			defer timeout()
			method, err := portable.NewMethod[string, string](portable.MethodConfig{Name: "write", Kind: portable.Command, Target: "document"})
			if err != nil {
				t.Fatal(err)
			}
			call, cancel := context.WithCancelCause(ctx)
			defer cancel(nil)
			cause := &Refusal{Code: "local", Message: "cancelled after the reply arrived"}
			remote, err := value.EncodeJSON(Refusal{Code: InvocationConflictCode, Message: "original backend refusal"})
			if err != nil {
				t.Fatal(err)
			}
			service := managedServiceOverPipe(t, ctx, method, func(context.Context, *managedParams) (*managedResult, error) {
				if refused {
					return &managedResult{Failure: remote.Bytes()}, nil
				}
				return &managedResult{Value: jsontext.Value(`"accepted"`)}, nil
			}, func(next sdk.MethodHandler) sdk.MethodHandler {
				return func(ctx context.Context, name string, request sdk.Request) (sdk.Result, error) {
					result, err := next(ctx, name, request)
					if name == managedInvoke && err == nil {
						cancel(cause)
					}
					return result, err
				}
			})
			input, err := value.EncodeJSON("original input")
			if err != nil {
				t.Fatal(err)
			}
			output, err := service.Invoke(call, commandInvocation(t, "original-command", method.Name(), input))
			if context.Cause(call) != cause || errors.Is(err, process.ErrOutcomeUnknown) || errors.Is(err, cause) {
				t.Fatalf("local cancellation replaced validated remote evidence: %v", err)
			}
			if refused {
				failure, ok := errors.AsType[*Refusal](err)
				if !ok || failure.Code != InvocationConflictCode || failure.Message != "original backend refusal" {
					t.Fatalf("validated refusal was lost: %v", err)
				}
			} else if err != nil || output.String() != `"accepted"` {
				t.Fatalf("validated result was lost: %s %v", output, err)
			}
		})
	}
}

func TestManagedStreamConsumerCannotCreateBackendRefusal(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(fmt.Sprintf("panic=%t", panics), func(t *testing.T) {
			ctx, timeout := context.WithTimeout(t.Context(), 5*time.Second)
			defer timeout()
			method, err := portable.NewStreamingMethod[string, string, string](portable.MethodConfig{Name: "write", Kind: portable.Command, Target: "document"})
			if err != nil {
				t.Fatal(err)
			}
			chunk, err := value.EncodeJSON("content")
			if err != nil {
				t.Fatal(err)
			}
			receivers := make(chan func(context.Context, uint64, value.JSON) error, 1)
			service := managedServiceOverPipe(t, ctx, method, func(ctx context.Context, params *managedParams) (*managedResult, error) {
				if params.CommandID != "original-command" || len(params.Meta[managedStreamKey]) == 0 {
					return nil, errors.New("missing original command or stream grant")
				}
				receive := <-receivers
				if err := receive(ctx, 1, chunk); err != nil {
					return nil, err
				}
				return &managedResult{Value: chunk.Bytes()}, nil
			})
			released := make(chan struct{})
			release := sync.OnceFunc(func() { close(released) })
			service.openStream = func(_ context.Context, _ *sdk.ClientSession, receive func(context.Context, uint64, value.JSON) error) (ManagedCallback, error) {
				receivers <- receive
				return ManagedCallback{
					Endpoint: "http://127.0.0.1:1/", Credential: strings.Repeat("a", managedCallbackTokenBytes*2), Call: strings.Repeat("b", managedCallbackTokenBytes*2),
					MaxResponseBytes: 1024, Released: released, Revoke: func() error { release(); return nil },
				}, nil
			}
			local := &Refusal{Code: InvocationConflictCode, Message: "private consumer refusal"}
			_, err = service.Stream(ctx, commandInvocation(t, "original-command", method.Name(), chunk), func(context.Context, value.JSON) error {
				if panics {
					panic(local)
				}
				return local
			})
			if !errors.Is(err, process.ErrOutcomeUnknown) || !errors.Is(err, local) {
				t.Fatalf("local consumer refusal became a known backend outcome: %v", err)
			}
			if panics && strings.Contains(err.Error(), local.Message) {
				t.Fatalf("consumer panic exposed its private cause: %v", err)
			}
			select {
			case <-released:
			default:
				t.Fatal("completed stream retained its original grant")
			}
		})
	}
}

func TestManagedStreamSeparatesReplyDeliveryAndCleanupEvidence(t *testing.T) {
	for _, refused := range []bool{false, true} {
		for _, phase := range []string{"delivery", "cleanup"} {
			t.Run(fmt.Sprintf("refused=%t/%s", refused, phase), func(t *testing.T) {
				ctx, timeout := context.WithTimeout(t.Context(), 5*time.Second)
				defer timeout()
				method, err := portable.NewStreamingMethod[string, string, string](portable.MethodConfig{Name: "write", Kind: portable.Command, Target: "document"})
				if err != nil {
					t.Fatal(err)
				}
				chunk, err := value.EncodeJSON("content")
				if err != nil {
					t.Fatal(err)
				}
				remote, err := value.EncodeJSON(Refusal{Code: "backend", Message: "original backend refusal"})
				if err != nil {
					t.Fatal(err)
				}
				local := &Refusal{Code: "local", Message: "local diagnostic after receipt"}
				call, cancel := context.WithCancelCause(ctx)
				defer cancel(nil)
				var receive func(context.Context, uint64, value.JSON) error
				service := managedServiceOverPipe(t, ctx, method, func(context.Context, *managedParams) (*managedResult, error) {
					if refused {
						return &managedResult{Failure: remote.Bytes()}, nil
					}
					return &managedResult{Value: jsontext.Value(`"accepted"`)}, nil
				}, func(next sdk.MethodHandler) sdk.MethodHandler {
					return func(ctx context.Context, name string, request sdk.Request) (sdk.Result, error) {
						result, err := next(ctx, name, request)
						if name == managedInvoke && err == nil {
							if phase == "cleanup" {
								cancel(local)
							} else if deliveryErr := receive(ctx, 1, chunk); !errors.Is(deliveryErr, local) {
								return nil, errors.New("raced delivery did not reach its original consumer")
							}
						}
						return result, err
					}
				})
				released := make(chan struct{})
				release := sync.OnceFunc(func() { close(released) })
				t.Cleanup(release)
				service.openStream = func(_ context.Context, _ *sdk.ClientSession, accepted func(context.Context, uint64, value.JSON) error) (ManagedCallback, error) {
					receive = accepted
					return ManagedCallback{
						Endpoint: "http://127.0.0.1:1/", Credential: strings.Repeat("a", managedCallbackTokenBytes*2), Call: strings.Repeat("b", managedCallbackTokenBytes*2),
						MaxResponseBytes: 1024, Released: released, Revoke: func() error {
							if phase != "cleanup" {
								release()
							}
							return nil
						},
					}, nil
				}
				output, err := service.Stream(call, commandInvocation(t, "original-command", method.Name(), chunk), func(context.Context, value.JSON) error { return local })
				if !errors.Is(err, local) {
					t.Fatalf("reply discarded its separate local diagnostic: %v", err)
				}
				if unknown := errors.Is(err, process.ErrOutcomeUnknown); unknown != (phase == "delivery" && !refused) {
					t.Fatalf("reply provenance was replaced by %s: unknown=%t error=%v", phase, unknown, err)
				}
				if refused {
					if failure, ok := errors.AsType[*Refusal](err); !ok || failure.Code != "backend" {
						t.Fatalf("known backend refusal was replaced: %v", err)
					}
				} else if phase == "cleanup" && output.String() != `"accepted"` {
					t.Fatalf("cleanup wait discarded validated output: %s %v", output, err)
				}
				if phase == "cleanup" && (!errors.Is(err, frame.ErrUnconfirmedRelease) || !errors.Is(err, context.Canceled)) {
					t.Fatalf("callback cleanup lost its original release boundary: %v", err)
				}
				release()
			})
		}
	}
}

func TestManagedReceiptRequiresAnOriginalValidatedReply(t *testing.T) {
	for _, test := range []struct {
		name    string
		receipt string
		found   bool
		failed  bool
		refused bool
	}{
		{name: "missing", receipt: `{"found":false}`},
		{name: "value", receipt: `{"found":true,"reply":{"value":"accepted"}}`, found: true},
		{name: "refusal", receipt: `{"found":true,"reply":{"failure":{"code":"declined","message":"backend refusal"}}}`, found: true, failed: true, refused: true},
		{name: "invalid-refusal", receipt: `{"found":true,"reply":{"failure":{"code":"","message":"invalid refusal"}}}`, failed: true},
		{name: "contradictory-reply", receipt: `{"found":true,"reply":{"value":"accepted","failure":{"code":"declined","message":"backend refusal"}}}`, failed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, timeout := context.WithTimeout(t.Context(), 5*time.Second)
			defer timeout()
			method, err := portable.NewMethod[string, string](portable.MethodConfig{Name: "write", Kind: portable.Command, Target: "document"})
			if err != nil {
				t.Fatal(err)
			}
			service := managedServiceOverPipe(t, ctx, method, func(_ context.Context, params *managedParams) (*managedResult, error) {
				if params.Method != managedReceipt || params.CommandID != "" {
					return nil, errors.New("receipt query changed the original invocation")
				}
				return &managedResult{Value: jsontext.Value(test.receipt)}, nil
			})
			service.receipt, err = portable.AdmitMethod(portable.MethodDescriptor{Name: managedReceipt, Kind: portable.Query, Target: "installation", Input: []byte(`{"type":"object"}`), Output: []byte(`{"type":"object"}`)})
			if err != nil {
				t.Fatal(err)
			}
			input, err := value.EncodeJSON("original input")
			if err != nil {
				t.Fatal(err)
			}
			output, found, err := service.ReadReceipt(ctx, commandInvocation(t, "original-command", method.Name(), input))
			if found != test.found || (err != nil) != test.failed {
				t.Fatalf("receipt evidence: found=%t result=%s error=%v", found, output, err)
			}
			if _, refused := errors.AsType[*Refusal](err); refused != test.refused {
				t.Fatalf("receipt refusal provenance changed: %v", err)
			}
			if found && err == nil && output.String() != `"accepted"` {
				t.Fatalf("receipt lost the validated output: %s", output)
			}
		})
	}
}
