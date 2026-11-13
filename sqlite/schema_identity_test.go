package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	framehost "github.com/Tangerg/frame/host"
)

func TestOwnerSchemaIdentityPreservesDeclarationBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owners.db")
	references := func(context.Context, Reader, framehost.InstallationID) ([]framehost.Dependency, error) {
		return nil, nil
	}
	left := `CREATE TABLE left_facts (value TEXT);`
	right := `CREATE TABLE right_facts (value TEXT);`
	original, err := Open(t.Context(), Config{Path: path, Admission: framehost.NewAdmission(), Owners: []OwnerSchema{
		{ID: "a", Statements: []string{left}, References: references},
		{ID: "b", Statements: []string{right}, References: references},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	ordered, err := Open(t.Context(), Config{Path: path, Admission: framehost.NewAdmission(), Owners: []OwnerSchema{
		{ID: "b", Statements: []string{right}, References: references},
		{ID: "a", Statements: []string{left}, References: references},
	}})
	if err != nil {
		t.Fatalf("input ordering changed the original identity: %v", err)
	}
	if err := ordered.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), Config{Path: path, Admission: framehost.NewAdmission(), Owners: []OwnerSchema{
		{ID: "a", Statements: []string{left + "\n-- owner b\n" + right}, References: references},
	}})
	if closeErr := reopened.Close(); closeErr != nil {
		t.Error(closeErr)
	}
	if err == nil {
		t.Fatal("owner declaration identity admitted a different persisted schema grant")
	}
}
