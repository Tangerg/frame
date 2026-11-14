package host_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	framehost "github.com/Tangerg/frame/host"
)

func TestPublicationPreparationCancelsItsAdmissionWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := testHost(t, framehost.Config{})
		point := metadataTestPoint[string](t, host)
		var instance string
		plugin := installNative(t, host, "provider", framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
			Setup: func(_ context.Context, setup framehost.Setup) ([]framehost.ExportValue, error) {
				instance = setup.InstanceID()
				return []framehost.ExportValue{framehost.Contribute("member", point, "original", "value")}, nil
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
			_, err := host.PreparePublication(ctx, instance, framehost.Contribute("dynamic", point, "next", "value"))
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
