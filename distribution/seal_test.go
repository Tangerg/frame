package distribution

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/value"
)

func TestCachedArtifactDriftRequiresExplicitPublicationRepair(t *testing.T) {
	_, _, archive, _ := packageFixture(t)
	store := storeFixture(t, nil)
	release := installArchive(t, store, archive)
	path := filepath.Join(store.directory, release.Digest.String(), "theme.css")
	if err := os.Chmod(path, privateFileMode); err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acquire(t.Context(), release.Digest)
	if lease != nil {
		_ = lease.Close()
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("writable cached artifact was admitted: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != privateFileMode {
		t.Fatalf("acquire changed the cached artifact: %v", err)
	}
	file := filepath.Join(t.TempDir(), "original.zip")
	if err := os.WriteFile(file, archive, privateFileMode); err != nil {
		t.Fatal(err)
	}
	_, importErr := store.ImportArchive(t.Context(), file, value.Hash(archive))
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	t.Logf("reimport error=%v; final artifact mode=%o", importErr, info.Mode().Perm())
	if importErr != nil || info.Mode().Perm() != sealedFileMode {
		t.Fatal("successful cached import returned a mutable artifact as a sealed release")
	}
}
