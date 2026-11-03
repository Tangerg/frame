package mcp

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

func TestManagedFingerprintIsIndependentOfFrameProjection(t *testing.T) {
	member, err := portable.NewMethod[string, string](portable.MethodConfig{Name: "query", Kind: portable.Query, Target: "entry", Description: "Read an entry."})
	if err != nil {
		t.Fatal(err)
	}
	raw := managedDescriptor{managedVersion, "collection.v1", "selection", "installation", []portable.MethodDescriptor{member.Descriptor()}}
	parse := func(d managedDescriptor) (*ManagedContract, jsontext.Value) {
		t.Helper()
		data, err := value.EncodeJSON(d)
		if err != nil {
			t.Fatal(err)
		}
		c, err := ParseManagedContract(data.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return c, data.Bytes()
	}
	want, wire := parse(raw)
	raw.Scope = "session"
	drift, _ := parse(raw)
	if want.capability.Identity() != drift.capability.Identity() || want.wire == drift.wire {
		t.Fatal("conflated Frame and Qimu contracts")
	}
	description := managedDescription{Protocol: managedVersion, Methods: raw.Methods}
	description.Capabilities = append(description.Capabilities, struct {
		Name     string         `json:"name"`
		Contract jsontext.Value `json:"contract"`
	}{"reader", wire})
	if _, err := description.verify(map[string]*ManagedContract{"reader": drift}); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("scope drift: %v", err)
	}
	raw.Scope = "installation"
	raw.Methods[0].Description = "Updated help."
	updated, _ := parse(raw)
	if updated.wire != want.wire {
		t.Fatal("presentation changed the wire identity")
	}
	description.Methods[0].Target = "pipeline"
	if _, err := description.verify(map[string]*ManagedContract{"reader": want}); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("member drift: %v", err)
	}
}

func TestManagedCallbackAddressRetainsOriginalUnit(t *testing.T) {
	routes := make(managedRoutes)
	source := frame.ExportRef{InstallationID: "files", UnitID: "first", Release: value.Hash([]byte("release")), Export: "read"}
	contract, _ := value.EncodeJSON("pinned")
	if _, err := routes.bind(source, contract); err != nil {
		t.Fatal(err)
	}
	if _, err := routes.bind(source, contract); err != nil {
		t.Fatal(err)
	}
	source.UnitID = "second"
	if _, err := routes.bind(source, contract); !errors.Is(err, frame.ErrContractMismatch) {
		t.Fatalf("ambiguous route: %v", err)
	}
}

func TestManagedLeaseWaitsForCallbackExitAfterRevocationFailure(t *testing.T) {
	lifetime := frame.NewLifetime(t.Context())
	lease, err := reserveManagedLease(lifetime)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	failure := errors.New("callbacks still active")
	lease.bind(&ManagedCallback{Released: released, Revoke: func() error { return failure }})
	if err := lease.finish(t.Context()); !errors.Is(err, frame.ErrUnconfirmedRelease) || !errors.Is(err, failure) {
		t.Fatal(err)
	}
	retirement := lifetime.BeginClose(errors.New("retire"))
	out, err := retirement.Wait(t.Context())
	if !errors.Is(err, frame.ErrUnconfirmedRelease) || out.Released || len(out.Pending) != 1 {
		t.Fatalf("lost callback owner %+v %v", out, err)
	}
	close(released)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	out, err = retirement.WaitReleased(ctx)
	if errors.Is(err, frame.ErrUnconfirmedRelease) || !out.Released {
		t.Fatalf("late callback exit %+v %v", out, err)
	}
}

func TestManagedChunksCannotAdvanceAfterRejection(t *testing.T) {
	method, err := portable.AdmitMethod(portable.MethodDescriptor{Name: "read", Kind: portable.Query, Target: "entry", Description: "Read.", Input: jsontext.Value(`{"type":"string"}`), Stream: jsontext.Value(`{"type":"integer"}`), Output: jsontext.Value(`{"type":"boolean"}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		sequence uint64
		chunk    string
	}{{"out-of-order", 2, "1"}, {"wrong type", 1, `"value"`}, {"over budget", 1, "12345"}} {
		t.Run(tc.name, func(t *testing.T) {
			call, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			invoked := false
			delivery := managedDelivery{call: call, abort: cancel, method: method, maxBytes: 4, emit: func(context.Context, value.JSON) error { invoked = true; return nil }}
			chunk, err := value.ParseJSON([]byte(tc.chunk))
			if err != nil {
				t.Fatal(err)
			}
			if err := delivery.receive(t.Context(), tc.sequence, chunk); err == nil || context.Cause(call) == nil || invoked {
				t.Fatalf("chunk %v, invoked %v", err, invoked)
			}
			valid, _ := value.EncodeJSON(1)
			if err := delivery.receive(t.Context(), 1, valid); err == nil || invoked {
				t.Fatal("escaped original stream")
			}
		})
	}
}

func TestManagedCallbackMetadataPreservesInvocationAndOtherGrant(t *testing.T) {
	done := make(chan struct{})
	grant := ManagedCallback{Endpoint: "http://127.0.0.1:16161/", Credential: strings.Repeat("a", 64), Call: strings.Repeat("b", 64), MaxResponseBytes: 1024, Released: done, Revoke: func() error { return nil }}
	var params sdk.ParamsBase
	if err := params.Set(managedInvocation, "original"); err != nil {
		t.Fatal(err)
	}
	if err := grant.attach(&params, managedCallback); err != nil {
		t.Fatal(err)
	}
	if err := grant.attach(&params, managedStreamKey); err != nil {
		t.Fatal(err)
	}
	if len(params.Meta) != 3 || string(params.Meta[managedInvocation]) != `"original"` {
		t.Fatal("grant replaced invocation evidence")
	}
	if strings.Contains(string(params.Meta[managedCallback]), "Released") {
		t.Fatal("leaked local owner")
	}
	grant.Endpoint = "http://example.org:80/"
	if err := grant.attach(&params, managedCallback); err == nil {
		t.Fatal("accepted non-loopback callback")
	}
}

func TestManagedLeaseKeepsUnprovenBrokerFailuresPending(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func() (*ManagedCallback, error)
	}{
		{"issue-panic", func() (*ManagedCallback, error) { panic("secret") }},
		{"partial-grant", func() (*ManagedCallback, error) {
			return issuedGrant(ManagedCallback{Credential: "secret"}, errors.New("failed"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lifetime := frame.NewLifetime(t.Context())
			lease, err := reserveManagedLease(lifetime)
			if err != nil {
				t.Fatal(err)
			}
			_, err = lease.issue(tc.open)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid diagnostic: %v", err)
			}
			if err := lease.finish(t.Context()); !errors.Is(err, frame.ErrUnconfirmedRelease) {
				t.Fatal(err)
			}
			out, err := lifetime.Close(t.Context())
			if out.Released || !errors.Is(err, frame.ErrUnconfirmedRelease) {
				t.Fatalf("lost uncertain grant %+v %v", out, err)
			}
		})
	}
	lifetime := frame.NewLifetime(t.Context())
	lease, err := reserveManagedLease(lifetime)
	if err != nil {
		t.Fatal(err)
	}
	proof := make(chan struct{})
	lease.bind(&ManagedCallback{Released: proof, Revoke: func() error { panic("secret") }})
	if err := lease.finish(t.Context()); !errors.Is(err, frame.ErrUnconfirmedRelease) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("revocation: %v", err)
	}
	close(proof)
	out, err := lifetime.BeginClose(errors.New("retire")).WaitReleased(t.Context())
	if !out.Released || errors.Is(err, frame.ErrUnconfirmedRelease) {
		t.Fatalf("proof lost %+v %v", out, err)
	}
}

func TestManagedCallGrantDistinguishesAliasesOfOnePhysicalExport(t *testing.T) {
	member, err := portable.NewMethod[string, string](portable.MethodConfig{Name: "read", Kind: portable.Command, Target: "document", Description: "Read an entry."})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := value.EncodeJSON(managedDescriptor{managedVersion, "documents.v1", "selection", "installation", []portable.MethodDescriptor{member.Descriptor()}})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := ParseManagedContract(descriptor.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	registered, err := frame.NewPortableContract[process.Service]("1", contract.Capability())
	if err != nil {
		t.Fatal(err)
	}
	ref := registered.Ref()
	unit := frame.UnitRef{InstallationID: "installed", Release: value.Hash([]byte("original")), UnitID: "reader"}
	approved := frame.ExportRef{InstallationID: unit.InstallationID, Release: unit.Release, UnitID: unit.UnitID, Export: "private", Contract: ref}
	denied := errors.New("grant denied")
	recognized := errors.New("authorized source reached broker")
	session := new(sdk.ClientSession)
	const id = "original-command"
	input, err := value.EncodeJSON("doc")
	if err != nil {
		t.Fatal(err)
	}
	c := managedConnection{session: session, contracts: map[string]*ManagedContract{"reader": contract},
		contributions: []managedContribution{{ID: "public", Export: "reader"}, {ID: "private", Export: "reader"}},
		values:        []frame.ExportValue{{Name: "public", Contract: ref}, {Name: "private", Contract: ref}},
		openCall: func(_ context.Context, actual *sdk.ClientSession, call ManagedCall) (*ManagedCallback, error) {
			if actual != session || call.Method.Name() != member.Name() || call.Input != input || call.CommandID != id {
				t.Fatal("session or method changed")
			}
			if call.Source == approved {
				return nil, recognized
			}
			return nil, denied
		},
	}
	lifetime := frame.NewLifetime(t.Context())

	for i, exported := range c.bindExports(unit, lifetime, nil) {
		service := exported.Value.(*ManagedService)
		if service.physical != "reader" {
			t.Fatal("different physical backend")
		}
		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := service.Invoke(cancelled, commandInvocation(t, id, member.Name(), input)); !errors.Is(err, context.Canceled) || errors.Is(err, process.ErrOutcomeUnknown) {
			t.Fatalf("cancelled invocation reached the broker or SDK: %v", err)
		}
		_, err := service.Invoke(t.Context(), commandInvocation(t, id, member.Name(), input))
		if !errors.Is(err, []error{denied, recognized}[i]) {
			t.Fatalf("grant for %s: %v", exported.Name, err)
		}
	}
	unit.InstallationID = "another"
	exported := c.bindExports(unit, lifetime, nil)[1]
	if _, err := exported.Value.(*ManagedService).Invoke(t.Context(), commandInvocation(t, id, member.Name(), input)); !errors.Is(err, denied) {
		t.Fatalf("other installation inherited permission: %v", err)
	}
	if out, err := lifetime.Close(t.Context()); err != nil || !out.Released {
		t.Fatalf("retire %+v %v", out, err)
	}
}
