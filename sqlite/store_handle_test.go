package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestStoreHandlesKeepIssuedCapabilitiesWithTheirOriginalAuthority(t *testing.T) {
	for _, replacement := range []string{"zero", "another authority"} {
		t.Run(replacement, func(t *testing.T) {
			admission := framehost.NewAdmission()
			config := sqlite.Config{
				Path:      filepath.Join(t.TempDir(), "authority.db"),
				Admission: admission,
				Owners: []sqlite.OwnerSchema{{
					ID:         "business",
					Statements: []string{`CREATE TABLE facts (value TEXT NOT NULL)`},
					References: func(context.Context, sqlite.Reader, framehost.InstallationID) ([]framehost.Dependency, error) {
						return nil, nil
					},
				}},
			}
			store, err := sqlite.Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			copied := store
			t.Cleanup(func() {
				if err := copied.Close(); err != nil {
					t.Error(err)
				}
			})
			owner, err := store.Owner("business")
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := store.ReserveRealmReceipt(t.Context(), "original-operation", value.Hash([]byte("request")), 128)
			if err != nil {
				t.Fatal(err)
			}
			id, epoch := store.ID(), store.Epoch()
			if replacement == "zero" {
				store = sqlite.Store{}
			} else {
				config.Path = filepath.Join(t.TempDir(), "other.db")
				config.Admission = framehost.NewAdmission()
				other, err := sqlite.Open(t.Context(), config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := other.Close(); err != nil {
						t.Error(err)
					}
				})
				store = other
			}
			if err := owner.Update(t.Context(), func(ctx context.Context, tx sqlite.Executor) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO facts VALUES ('original authority')`)
				return err
			}); err != nil {
				t.Fatalf("replacing the caller's Store changed an issued Owner: %v", err)
			}
			if err := reservation.Record(t.Context(), []byte("original receipt")); err != nil {
				t.Fatalf("replacing the caller's Store changed an issued reservation: %v", err)
			}
			if copied.ID() != id || copied.Epoch() != epoch || copied.Admission() != admission {
				t.Fatal("copy lost its original authority identity")
			}
			var fact string
			if err := copied.Read(t.Context(), func(reader sqlite.Reader) error {
				return reader.QueryRowContext(t.Context(), `SELECT value FROM facts`).Scan(&fact)
			}); err != nil || fact != "original authority" {
				t.Fatalf("copy lost the original database: %q, %v", fact, err)
			}
			if receipt, err := copied.RealmReceipt(t.Context(), "original-operation"); err != nil || string(receipt) != "original receipt" {
				t.Fatalf("copy lost the original receipt: %q, %v", receipt, err)
			}
			if err := copied.Close(); err != nil {
				t.Fatal(err)
			}
			if err := owner.Update(t.Context(), func(context.Context, sqlite.Executor) error {
				t.Fatal("issued capability outlived its original authority")
				return nil
			}); !errors.Is(err, frame.ErrClosed) {
				t.Fatalf("closed authority admitted an issued capability: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("zero handle retained another authority: %v", err)
			}
		})
	}
}
