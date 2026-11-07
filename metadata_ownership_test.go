package frame_test

import (
	"context"
	"github.com/Tangerg/frame"
	"testing"
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
	host := testHost(t, frame.HostConfig{})
	point, err := frame.NewPoint[mutatingMetadata, string]("encoding.metadata", "1")
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(point); err != nil {
		t.Fatal(err)
	}
	plugin := installNative(t, host, "metadata", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "member", Contract: point.Ref()}}},
		Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) {
			return []frame.ExportValue{frame.Contribute("member", point, mutatingMetadata{Items: []string{"before"}}, "value")}, nil
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
	before, err := frame.Candidates(snapshot, point)
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
	after, err := frame.Candidates(fresh, point)
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
