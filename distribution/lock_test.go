package distribution

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestParseLockValidatesItsOriginalInventory(t *testing.T) {
	digest := value.Hash([]byte("artifact"))
	contract, err := frame.NewContract[string]("lock.contract", "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		artifacts []Artifact
		contracts []frame.ContractRef
	}{
		{name: "escaping path", artifacts: []Artifact{{Path: "../escape", Digest: digest}}},
		{name: "negative size", artifacts: []Artifact{{Path: "asset.txt", Digest: digest, Size: -1}}},
		{name: "duplicate path", artifacts: []Artifact{{Path: "asset.txt", Digest: digest}, {Path: "asset.txt", Digest: digest}}},
		{name: "incomplete contract", contracts: []frame.ContractRef{{Name: "incomplete"}}},
		{name: "duplicate contract", contracts: []frame.ContractRef{contract.Ref(), contract.Ref()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lock := Lock{SchemaVersion: LockSchemaVersion, Package: "lock.package", Release: digest, Artifacts: test.artifacts, Contracts: test.contracts}
			encoded, err := json.Marshal(lock)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseLock(encoded); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid inventory was admitted: %v", err)
			}
		})
	}
}

func TestManifestLockRoundTripPreservesItsProjection(t *testing.T) {
	_, manifest, _, lock := packageFixture(t)
	manifest.Artifacts = lock.Artifacts
	encoded, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseLock(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.Verify(manifest); err != nil {
		t.Fatal(err)
	}
}
