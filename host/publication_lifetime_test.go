package host_test

import (
	"context"
	"testing"
	"testing/synctest"

	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

type blockedPublicationMetadata struct {
	value.JSON
	entered chan struct{}
	resume  chan struct{}
}

func (m blockedPublicationMetadata) MarshalJSON() ([]byte, error) {
	if m.entered != nil {
		close(m.entered)
		<-m.resume
	}
	return m.JSON.MarshalJSON()
}

func TestPublicationPreparationRetainsItsInstanceUntilCallbackReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		host := testHost(t, framehost.Config{})
		point := metadataTestPoint[blockedPublicationMetadata](t, host)
		encoded, err := value.ParseJSON([]byte(`"metadata"`))
		if err != nil {
			t.Fatal(err)
		}
		var instance string
		cleaned := make(chan struct{})
		plugin := installNative(t, host, "publisher", framehost.Unit{
			Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
			Setup: func(_ context.Context, setup *framehost.Setup) ([]framehost.ExportValue, error) {
				instance = setup.InstanceID()
				if _, err := setup.Cleanup("original provider", func() error { close(cleaned); return nil }); err != nil {
					return nil, err
				}
				return []framehost.ExportValue{framehost.Contribute("member", point, blockedPublicationMetadata{JSON: encoded}, "value")}, nil
			},
		})
		if _, err := host.Activate(t.Context(), source("publisher", plugin, "main", "member", point.Ref())); err != nil {
			t.Fatal(err)
		}
		entered, resume := make(chan struct{}), make(chan struct{})
		finished := make(chan error, 1)
		go func() {
			request, err := host.PreparePublication(t.Context(), instance, framehost.Contribute("dynamic", point, blockedPublicationMetadata{JSON: encoded, entered: entered, resume: resume}, "value"))
			if request != nil {
				t.Error("retired instance produced publication authority")
			}
			finished <- err
		}()
		<-entered
		if err := host.Retire(t.Context(), "publisher"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-cleaned:
			t.Error("provider cleaned while its metadata callback was still running")
		default:
		}
		close(resume)
		if err := <-finished; err == nil {
			t.Fatal("publication preparation accepted a retired instance")
		}
		if err := host.WaitInstanceRetired(t.Context(), instance); err != nil {
			t.Fatal(err)
		}
		select {
		case <-cleaned:
		default:
			t.Fatal("provider did not clean after its original callback returned")
		}
	})
}
