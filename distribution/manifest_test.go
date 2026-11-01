package distribution

import (
	json "encoding/json/v2"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestContractRoleIsPartOfNominalIdentity(t *testing.T) {
	service := frame.ContractRef{Name: "example/report", Version: "1", Digest: value.Hash([]byte("service")), Kind: frame.ServiceKind, Mode: frame.PortableMode}
	point := service
	point.Kind, point.Digest = frame.PointKind, value.Hash([]byte("point"))
	manifest := Manifest{SchemaVersion: 1, Package: "example.report", Version: "1", Contracts: []frame.ContractRef{service, point}, Units: []Unit{{ID: "report", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "report", Contract: point}}}}, Contributions: []Contribution{{Unit: "report", Export: "report", Point: point, Key: "report"}}}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("service and consuming point cannot share their nominal name: %v", err)
	}
	changed := service
	changed.Digest = value.Hash([]byte("changed service"))
	manifest.Contracts = append(manifest.Contracts, changed)
	if err := manifest.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("one nominal service version admitted two descriptors: %v", err)
	}
	manifest.Contracts = manifest.Contracts[:2]
	manifest.Units[0].Exports[0].Contract = service
	if err := manifest.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a service export was relabeled as a point contribution: %v", err)
	}
}

func TestExplicitNullContributionMetadataSurvivesAuthorEncoding(t *testing.T) {
	point := frame.ContractRef{Name: "example/actions", Version: "1", Digest: value.Hash([]byte("point")), Kind: frame.PointKind, Mode: frame.PortableMode}
	manifest := Manifest{SchemaVersion: 1, Package: "example.actions", Version: "1", Artifacts: []Artifact{}, Contracts: []frame.ContractRef{point}, Units: []Unit{{ID: "actions", Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "action", Contract: point}}}}, Contributions: []Contribution{{Unit: "actions", Export: "action", Point: point, Key: "action", Metadata: []byte("null")}}}
	original, err := manifest.Identity()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := decoded.Identity()
	if err != nil || original != actual || string(decoded.Contributions[0].Metadata) != "null" {
		t.Fatalf("the author writer changed explicit null to omission: %s, %v", encoded, err)
	}
}
