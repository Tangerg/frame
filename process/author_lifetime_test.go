package process

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/frame"
)

func TestServeRetirementJoinsPreparationBeforeResourceCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started, finish, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{})
		unit := Unit{ID: "author", Prepare: func(ctx context.Context, setup Setup) (Prepared, error) {
			if _, err := setup.Lifetime.Cleanup("prepared resource", func() error {
				close(cleaned)
				return nil
			}); err != nil {
				return Prepared{}, err
			}
			close(started)
			<-finish
			return Prepared{}, context.Cause(ctx)
		}}
		client, server := net.Pipe()
		served := make(chan error, 1)
		go func() { served <- Serve(ctx, server, unit) }()
		host := newPeer(client, wire.MaxFrameBytes, nil)
		host.start()
		defer host.end(context.Canceled)
		var description description
		if err := host.request(context.Background(), wire.Methods.Hello, hello{wire.Version, wire.MaxFrameBytes}, &description); err != nil {
			t.Fatal(err)
		}
		prepared := make(chan error, 1)
		go func() {
			prepared <- host.acknowledge(context.Background(), wire.Methods.Prepare, preparation{Imports: map[string][]offered{}})
		}()
		<-started
		cancel()
		synctest.Wait()
		select {
		case <-cleaned:
			t.Error("retirement cleaned a resource while its preparation was still using it")
		default:
		}
		select {
		case err := <-served:
			t.Errorf("serve returned before preparation joined: %v", err)
		default:
		}
		close(finish)
		if err := <-served; !errors.Is(err, context.Canceled) {
			t.Fatalf("serve lost cancellation: %v", err)
		}
		<-prepared
		select {
		case <-cleaned:
		default:
			t.Fatal("joined preparation left its resource uncleaned")
		}
		<-host.joined
	})
}

func TestPreparationDoesNotRunAfterItsLifetimeRetires(t *testing.T) {
	lifetime := frame.NewLifetime(t.Context())
	if _, err := lifetime.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	called := false
	s := &server{state: described, life: lifetime, unit: Unit{Prepare: func(context.Context, Setup) (Prepared, error) {
		called = true
		return Prepared{}, nil
	}}}
	if _, err := s.prepare(t.Context(), []byte(`{"imports":{}}`)); !errors.Is(err, frame.ErrClosed) {
		t.Fatalf("retired preparation admitted: %v", err)
	}
	if called {
		t.Fatal("retired lifetime executed plugin preparation")
	}
}

func TestActivationCannotAcknowledgeItsRetiredLifetime(t *testing.T) {
	lifetime := frame.NewLifetime(t.Context())
	s := &server{state: prepared, life: lifetime, activate: func(context.Context) error {
		lifetime.BeginClose(frame.ErrClosed)
		return nil
	}}
	if _, err := s.activateUnit(t.Context(), []byte(`{}`)); err == nil {
		t.Fatal("activation acknowledged after its original lifetime closed admission")
	}
	if _, err := lifetime.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleCannotAcknowledgeCancelledWork(t *testing.T) {
	for _, method := range []string{wire.Methods.Prepare, wire.Methods.Activate} {
		t.Run(method, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			cause := errors.New("original lifecycle request cancelled")
			lifetime := frame.NewLifetime(t.Context())
			defer lifetime.Close(t.Context())
			s := &server{state: described, life: lifetime, unit: Unit{
				Prepare: func(context.Context, Setup) (Prepared, error) {
					cancel(cause)
					return Prepared{}, nil
				},
			}, activate: func(context.Context) error {
				cancel(cause)
				return nil
			}}
			payload := []byte(`{"imports":{}}`)
			if method == wire.Methods.Activate {
				s.state = prepared
				payload = []byte(`{}`)
			}
			if _, err := s.handle(ctx, method, payload); !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
				t.Fatalf("lifecycle acknowledged cancelled work or lost its cause: %v", err)
			}
		})
	}
}
