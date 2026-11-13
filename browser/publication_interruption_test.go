package browser_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type interruptedPublicationMetadata string

var errPublicationCodec = errors.New("publication metadata interrupted")

func (m interruptedPublicationMetadata) MarshalJSON() ([]byte, error) { return json.Marshal(string(m)) }
func (interruptedPublicationMetadata) JSONSchemaModel() any           { return "" }
func (m *interruptedPublicationMetadata) UnmarshalJSON(raw []byte) error {
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	switch decoded {
	case "panic":
		panic(errPublicationCodec)
	case "goexit":
		runtime.Goexit()
	}
	*m = interruptedPublicationMetadata(decoded)
	return nil
}

func TestPublicationInterruptionRetainsOriginalFailure(t *testing.T) {
	for _, interruption := range []string{"panic", "goexit"} {
		t.Run(interruption, func(t *testing.T) {
			point, err := framehost.NewPoint[interruptedPublicationMetadata, browser.Object]("test.interrupted-publication", "1")
			if err != nil {
				t.Fatal(err)
			}
			e := openBridge(t, filepath.Join(t.TempDir(), "state.db"), true)
			if err := e.host.Register(point); err != nil {
				t.Fatal(err)
			}
			e.plugin, err = framehost.Define(framehost.Definition{Revision: value.Hash([]byte("interrupted-publication")), Units: []framehost.Unit{{Declaration: framehost.UnitDeclaration{ID: "view", Profile: framehost.BrowserJS, Exports: []framehost.ExportDeclaration{{Name: "preview", Contract: e.first.Ref()}, {Name: "theme", Contract: e.second.Ref()}, {Name: "interrupted", Contract: point.Ref()}}}}}})
			if err != nil {
				t.Fatal(err)
			}
			plan, activated := e.beginActivation("publisher")
			report := e.readyReport(plan)
			for i := range report.Contributions {
				if report.Contributions[i].Object.Export.Contract == point.Ref() {
					report.Contributions[i].MetadataJSON = `"safe"`
				}
			}
			if result, err := e.bridge.Ready(e.ctx, e.credentials.Token, report); err != nil || result.Decision != browser.Accepted {
				t.Fatalf("Ready: %+v %v", result, err)
			}
			if err := <-activated; err != nil {
				t.Fatal(err)
			}
			request := browser.PublicationRequest{OperationID: "interrupted", Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution, Object: browser.ObjectRef{ObjectID: "dynamic-object", Export: browser.LocalExport{InstanceID: plan.Units[0].Instance.ID, Name: "dynamic", Contract: point.Ref()}}, MetadataJSON: `"` + interruption + `"`}
			done := make(chan struct{})
			var returned bool
			var panicked any
			go func() {
				defer close(done)
				defer func() { panicked = recover() }()
				_, _ = e.bridge.Publish(e.ctx, e.credentials.Token, request)
				returned = true
			}()
			select {
			case <-done:
			case <-e.ctx.Done():
				t.Fatal(e.ctx.Err())
			}
			if returned || (interruption == "panic" && panicked != errPublicationCodec) || (interruption == "goexit" && panicked != nil) {
				t.Fatalf("unexpected interruption: returned=%v panic=%v", returned, panicked)
			}
			for range 2 {
				observed, err := e.bridge.Publication(e.ctx, e.credentials.Token, request.OperationID)
				if !errors.Is(err, framehost.ErrUnavailable) || observed.Publication != nil {
					t.Fatalf("interrupted publication appeared successful: %+v, %v", observed, err)
				}
			}
			replayed, err := e.bridge.Publish(e.ctx, e.credentials.Token, request)
			if !errors.Is(err, framehost.ErrUnavailable) || replayed.Publication != nil {
				t.Fatalf("interrupted publication replayed as success: %+v, %v", replayed, err)
			}
			changed := request
			changed.MetadataJSON = `"safe"`
			if _, err := e.bridge.Publish(e.ctx, e.credentials.Token, changed); !errors.Is(err, sqlite.ErrConflict) {
				t.Fatalf("interrupted operation admitted another request: %v", err)
			}
			if _, err := e.host.Publication(plan.Units[0].Instance.ID, request.OperationID); !errors.Is(err, framehost.ErrUnavailable) {
				t.Fatalf("interrupted operation published: %v", err)
			}
			if _, err := e.store.RealmReceipt(e.ctx, request.OperationID); err != nil {
				t.Fatalf("receipt not archived: %v", err)
			}
		})
	}
}
