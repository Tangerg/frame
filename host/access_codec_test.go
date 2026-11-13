package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
)

type accessCodecMetadata string

type metadataAccessHook struct {
	call    func() error
	entered atomic.Bool
}

// A codec has no context parameter; this test hook connects its invocation to
// the one borrow being exercised. These tests do not run in parallel.
var activeMetadataAccess atomic.Pointer[metadataAccessHook]

func (m accessCodecMetadata) MarshalJSON() ([]byte, error) { return json.Marshal(string(m)) }
func (accessCodecMetadata) JSONSchemaModel() any           { return "" }
func (m *accessCodecMetadata) UnmarshalJSON(raw []byte) error {
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*m = accessCodecMetadata(decoded)
	if hook := activeMetadataAccess.Load(); hook != nil && hook.entered.CompareAndSwap(false, true) {
		return hook.call()
	}
	return nil
}

type metadataAccessFixture struct {
	host      framehost.Host
	owner     frame.Lifetime
	scope     framehost.ScopeGrant
	plan      framehost.BindingPlan
	selection framehost.Selection[accessCodecMetadata, string]
}

func newMetadataAccess(t *testing.T) metadataAccessFixture {
	t.Helper()
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	capability, err := frame.NewCapability("test.access-metadata-service", frame.MethodDescriptor{Name: "read", Kind: frame.Query, Input: []byte(`{"type":"string"}`), Output: []byte(`{"type":"string"}`)})
	if err != nil {
		t.Fatal(err)
	}
	service, err := framehost.NewPortableContract[string]("1", capability)
	if err != nil {
		t.Fatal(err)
	}
	point, err := framehost.NewPortablePoint[accessCodecMetadata]("test.access-metadata", "1", []byte(`{"type":"string"}`), service)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(service, point); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "metadata", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
		Setup: func(context.Context, *framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute("member", point, accessCodecMetadata("metadata"), "value")}, nil
		},
	})
	if _, err := host.Activate(ctx, source("metadata", plugin, "main", "member", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := framehost.Candidates(snapshot, point)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := framehost.Select(snapshot, point, "test.metadata-access", candidates...)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := framehost.Capture(snapshot, selection.Ref())
	if err != nil {
		t.Fatal(err)
	}
	owner, scope := ownerAndScope(t, host)
	return metadataAccessFixture{host, owner, scope, plan, selection}
}

func TestMetadataCodecCanReenterItsOriginalAccess(t *testing.T) {
	f := newMetadataAccess(t)
	ctx := testContext(t)
	reentered := make(chan error, 1)
	var copied framehost.Access
	defer activeMetadataAccess.Store(nil)
	err := f.host.With(ctx, f.scope, f.plan, func(access *framehost.Access) error {
		copied = *access
		hook := &metadataAccessHook{call: func() error {
			go func() {
				member, err := f.selection.One(access)
				if err == nil && (member.Metadata != "metadata" || member.Value != "value") {
					err = errors.New("reentered access changed its original member")
				}
				reentered <- err
			}()
			select {
			case err := <-reentered:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		activeMetadataAccess.Store(hook)
		_, err := f.selection.One(access)
		return err
	})
	activeMetadataAccess.Store(nil)
	if err != nil {
		t.Fatalf("codec could not reenter the original access: %v", err)
	}
	if _, err := f.selection.One(&copied); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("copied access survived its original borrow: %v", err)
	}
}

func TestBorrowJoinsAnAcceptedMetadataReadBeforeReleasingItsWork(t *testing.T) {
	f := newMetadataAccess(t)
	ctx := testContext(t)
	entered, release := make(chan struct{}), make(chan struct{})
	callbackReturned := make(chan struct{})
	read := make(chan error, 1)
	borrowed := make(chan error, 1)
	var copied framehost.Access
	activeMetadataAccess.Store(&metadataAccessHook{call: func() error {
		close(entered)
		<-release
		return nil
	}})
	defer activeMetadataAccess.Store(nil)
	go func() {
		borrowed <- f.host.With(ctx, f.scope, f.plan, func(access *framehost.Access) error {
			copied = *access
			go func() { _, err := f.selection.One(&copied); read <- err }()
			<-entered
			close(callbackReturned)
			return nil
		})
	}()
	select {
	case <-callbackReturned:
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	var early error
	finishedEarly := false
	select {
	case early = <-borrowed:
		finishedEarly = true
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	readErr := <-read
	if !finishedEarly {
		early = <-borrowed
	}
	if finishedEarly || early != nil {
		t.Fatalf("borrow did not join its original metadata read: early=%v, error=%v", finishedEarly, early)
	}
	if readErr != nil && !errors.Is(readErr, frame.ErrClosed) {
		t.Fatalf("accepted metadata read failed: %v", readErr)
	}
	if _, err := f.selection.One(&copied); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("closed access accepted another metadata read: %v", err)
	}
}

func TestMetadataReadUsesTheOriginalBorrowWorkBudget(t *testing.T) {
	f := newMetadataAccess(t)
	ctx := testContext(t)
	use, err := f.host.Acquire(ctx, f.plan, framehost.UseConfig{Owner: f.owner, Scope: f.scope})
	if err != nil {
		t.Fatal(err)
	}
	var held []*frame.Work
	defer func() {
		for _, work := range held {
			work.Finish()
		}
		if outcome, err := use.Close(ctx); err != nil || !outcome.Released {
			t.Errorf("Use cleanup: %+v, %v", outcome, err)
		}
	}()
	for {
		work, err := use.Enter(ctx)
		if errors.Is(err, frame.ErrCapacity) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, work)
	}
	if len(held) == 0 {
		t.Fatal("Use had no work budget")
	}
	held[len(held)-1].Finish()
	held = held[:len(held)-1]
	err = framehost.Borrow(ctx, use, f.selection, func(_ context.Context, values []string) error {
		if len(values) != 1 || values[0] != "value" {
			return errors.New("borrow lost its original selected value")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("one available Work could not admit one borrow: %v", err)
	}
}
