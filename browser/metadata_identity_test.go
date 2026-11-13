package browser_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

var readyMetadataBoundary func()

type foldedMetadata string

func (foldedMetadata) JSONSchemaModel() any { return "" }
func (m foldedMetadata) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(m))
}
func (m *foldedMetadata) UnmarshalJSON(raw []byte) error {
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	*m = foldedMetadata(strings.ToLower(decoded))
	return nil
}

type boundaryMetadata string

func (boundaryMetadata) JSONSchemaModel() any { return "" }
func (m boundaryMetadata) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(m))
}
func (m *boundaryMetadata) UnmarshalJSON(raw []byte) error {
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	if readyMetadataBoundary != nil {
		readyMetadataBoundary()
	}
	*m = boundaryMetadata(decoded)
	return nil
}

func TestReadyRevalidatesAfterMetadataPreparation(t *testing.T) {
	for _, boundary := range []struct {
		name          string
		cancelRequest bool
	}{
		{name: "cancellation", cancelRequest: true},
		{name: "recovery"},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			e := openBridge(t, filepath.Join(t.TempDir(), "metadata.db"), true)
			point, err := framehost.NewPoint[boundaryMetadata, browser.Object]("test.metadata-admission", "1")
			if err != nil {
				t.Fatal(err)
			}
			if err := e.host.Register(point); err != nil {
				t.Fatal(err)
			}
			e.plugin, err = framehost.Define(framehost.Definition{
				Revision: value.Hash([]byte("metadata-admission")), Units: []framehost.Unit{{Declaration: framehost.UnitDeclaration{
					ID: "view", Profile: framehost.BrowserJS, Exports: []framehost.ExportDeclaration{
						{Name: "preview", Contract: e.first.Ref()}, {Name: "theme", Contract: e.second.Ref()}, {Name: "guarded", Contract: point.Ref()},
					},
				}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			plan, activated := e.beginActivation("publisher")
			report := e.readyReport(plan)
			for index, contribution := range report.Contributions {
				if contribution.Object.Export.Contract == point.Ref() {
					report.Contributions[index].MetadataJSON = `"Original"`
				}
			}
			ctx, cancel := context.WithCancel(e.ctx)
			defer cancel()
			cause := framehost.ErrRecoveryRequired
			readyMetadataBoundary = func() { e.host.Admission().RequireRecovery(cause) }
			if boundary.cancelRequest {
				cause = context.Canceled
				readyMetadataBoundary = cancel
			}
			result, err := e.bridge.Ready(ctx, e.credentials.Token, report)
			readyMetadataBoundary = nil
			if !errors.Is(err, cause) || result.Decision == browser.Accepted {
				t.Fatalf("metadata preparation advanced Ready after %s: result=%+v, error=%v", boundary.name, result, err)
			}
			if err := e.host.Admission().Recover(e.ctx, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if result, err := e.bridge.Ready(e.ctx, e.credentials.Token, report); err != nil || result.Decision != browser.Accepted {
				t.Fatalf("original report did not resume: result=%+v, error=%v", result, err)
			}
			if err := <-activated; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPortablePublicationsPreserveOriginalWireMetadata(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "metadata.db"), true)
	capability, err := frame.NewCapability("test.metadata-value", frame.MethodDescriptor{
		Name: "read", Kind: frame.Query, Input: []byte(`{"type":"string"}`), Output: []byte(`{"type":"string"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := framehost.NewPortableContract[browser.Object]("1", capability)
	if err != nil {
		t.Fatal(err)
	}
	point, err := framehost.NewPortablePoint[foldedMetadata]("test.precise-metadata", "1", []byte(`{"type":"string"}`), service)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.host.Register(service, point); err != nil {
		t.Fatal(err)
	}
	e.plugin, err = framehost.Define(framehost.Definition{
		Revision: value.Hash([]byte("precise-metadata")),
		Units: []framehost.Unit{{Declaration: framehost.UnitDeclaration{
			ID: "view", Profile: framehost.BrowserJS, Exports: []framehost.ExportDeclaration{
				{Name: "preview", Contract: e.first.Ref()},
				{Name: "theme", Contract: e.second.Ref()},
				{Name: "precise", Contract: point.Ref()},
			},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, activated := e.beginActivation("publisher")
	report := e.readyReport(plan)
	const originalMetadata = `"Original"`
	for index, contribution := range report.Contributions {
		if contribution.Object.Export.Contract == point.Ref() {
			report.Contributions[index].MetadataJSON = originalMetadata
		}
	}
	if result, err := e.bridge.Ready(e.ctx, e.credentials.Token, report); err != nil || result.Decision != browser.Accepted {
		t.Fatalf("Ready: %+v, %v", result, err)
	}
	if err := <-activated; err != nil {
		t.Fatal(err)
	}
	request := browser.PublicationRequest{
		OperationID: "precise-dynamic", Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution,
		Object: browser.ObjectRef{ObjectID: "precise-object", Export: browser.LocalExport{
			InstanceID: plan.Units[0].Instance.ID, Name: "dynamic", Contract: point.Ref(),
		}},
		MetadataJSON: originalMetadata,
	}
	if result, err := e.bridge.Publish(e.ctx, e.credentials.Token, request); err != nil || result.Decision != browser.Accepted {
		t.Fatalf("Publish: %+v, %v", result, err)
	}
	snapshot, err := e.host.Snapshot(e.ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := snapshot.Candidates(point.Ref())
	if err != nil || len(candidates) != 2 {
		t.Fatalf("published metadata: candidates=%d, error=%v", len(candidates), err)
	}
	for _, candidate := range candidates {
		encoded, err := candidate.MetadataJSON()
		if err != nil || string(encoded) != originalMetadata {
			t.Errorf("Go projection rewrote %s metadata: got=%s, want=%s, error=%v", candidate.Source().Export, encoded, originalMetadata, err)
		}
	}
}
