package host_test

import (
	"context"
	"testing"

	framehost "github.com/Tangerg/frame/host"
)

type mutatingMetadata struct {
	Items []string `json:"items"`
}

func (m mutatingMetadata) MarshalJSON() ([]byte, error) {
	m.Items[0] = "after"
	return []byte(`{"items":["after"]}`), nil
}

func TestMetadataEncodingCannotMutatePublication(t *testing.T) {
	ctx := testContext(t)
	host := testHost(t, framehost.Config{})
	point, err := framehost.NewPoint[mutatingMetadata, string]("encoding.metadata", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "metadata", framehost.Unit{
		Declaration: framehost.UnitDeclaration{ID: "main", Profile: framehost.NativeGo, Exports: []framehost.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
		Setup: func(context.Context, framehost.Setup) ([]framehost.ExportValue, error) {
			return []framehost.ExportValue{framehost.Contribute("member", point, mutatingMetadata{Items: []string{"before"}}, "value")}, nil
		},
	})
	if _, err := host.Activate(ctx, source("metadata", plugin, "main", "member", point.Ref())); err != nil {
		t.Fatal(err)
	}
	snapshot, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	beforeRevision, err := snapshot.Revision(point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	before, err := framehost.Candidates(snapshot, point)
	if err != nil {
		t.Fatal(err)
	}
	beforeMetadata, err := before[0].Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if beforeMetadata.Items[0] != "before" {
		t.Fatal(beforeMetadata.Items)
	}
	refs, err := snapshot.Candidates(point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := refs[0].MetadataJSON(); err != nil {
		t.Fatal(err)
	}
	fresh, err := host.Snapshot(ctx, point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	freshRevision, err := fresh.Revision(point.Ref())
	if err != nil {
		t.Fatal(err)
	}
	after, err := framehost.Candidates(fresh, point)
	if err != nil {
		t.Fatal(err)
	}
	afterMetadata, err := after[0].Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if got := afterMetadata.Items[0]; got != "before" {
		t.Fatalf("metadata encoder mutated the admitted publication: metadata=%s revision before=%d after=%d", got, beforeRevision, freshRevision)
	}
}
