package frame_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/Tangerg/frame"
)

func TestPublicationPreparationCancelsItsAdmissionWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := testHost(t, frame.HostConfig{})
		point := metadataTestPoint[string](t, host)
		var instance string
		plugin := installNative(t, host, "provider", frame.Unit{
			Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
			Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
				instance = setup.InstanceID()
				return []frame.ExportValue{frame.Contribute("member", point, "original", "value")}, nil
			},
		})
		if _, err := host.Activate(t.Context(), source("provider", plugin, "main", "member", point.Ref())); err != nil {
			t.Fatal(err)
		}
		entered, resume, left := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			left <- host.Admission().Under(t.Context(), func() error {
				close(entered)
				<-resume
				return nil
			})
		}()
		<-entered
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		prepared := make(chan error, 1)
		go func() {
			_, err := host.PreparePublication(ctx, instance, frame.Contribute("dynamic", point, "next", "value"))
			prepared <- err
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-prepared:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("publication preparation lost caller cancellation: %v", err)
			}
		default:
			t.Error("publication preparation waited on instance lifetime after caller cancellation")
		}
		close(resume)
		if err := <-left; err != nil {
			t.Fatal(err)
		}
	})
}
