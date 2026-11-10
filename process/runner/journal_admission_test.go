package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame/value"
)

func TestOpenRejectsIncompleteJournalWithoutRewriting(t *testing.T) {
	valid, err := json.Marshal(journal{Version: journalVersion, Attempts: map[string]storedAttempt{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		data string
	}{
		{name: "empty object", data: `{}`},
		{name: "null", data: `null`},
		{name: "missing version", data: `{"epoch":7,"attempts":{}}`},
		{name: "null version", data: `{"version":null,"epoch":7,"attempts":{}}`},
		{name: "missing epoch", data: fmt.Sprintf(`{"version":%d,"attempts":{}}`, journalVersion)},
		{name: "null epoch", data: fmt.Sprintf(`{"version":%d,"epoch":null,"attempts":{}}`, journalVersion)},
		{name: "missing attempts", data: fmt.Sprintf(`{"version":%d,"epoch":7}`, journalVersion)},
		{name: "null attempts", data: fmt.Sprintf(`{"version":%d,"epoch":7,"attempts":null}`, journalVersion)},
		{name: "obsolete version", data: fmt.Sprintf(`{"version":%d,"epoch":7,"attempts":{}}`, journalVersion-1)},
		{name: "negative epoch", data: fmt.Sprintf(`{"version":%d,"epoch":-1,"attempts":{}}`, journalVersion)},
		{name: "unknown field", data: fmt.Sprintf(`{"version":%d,"epoch":0,"attempts":{},"extra":true}`, journalVersion)},
		{name: "duplicate field", data: fmt.Sprintf(`{"version":%d,"epoch":0,"epoch":7,"attempts":{}}`, journalVersion)},
		{name: "missing observation", data: fmt.Sprintf(`{"version":%d,"epoch":7,"attempts":{"original":{"requestDigest":%q}}}`, journalVersion, value.Hash(nil).String())},
		{name: "trailing object", data: string(valid) + `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Config{Directory: t.TempDir()}
			path := filepath.Join(config.Directory, journalFilename)
			original := []byte(test.data)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			server, err := Open(config)
			if server != nil {
				_ = server.Close()
			}
			if err == nil {
				t.Fatal("malformed persisted journal was accepted")
			}
			if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, original) {
				t.Fatalf("refused journal was rewritten: %q %v", current, err)
			}
			if err := os.WriteFile(path, valid, 0600); err != nil {
				t.Fatal(err)
			}
			server, err = Open(config)
			if err != nil {
				t.Fatalf("failed admission retained authority or rejected complete epoch zero: %v", err)
			}
			if err := server.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenCreatesAndRestoresExplicitInitialJournal(t *testing.T) {
	config := Config{Directory: t.TempDir()}
	for range 2 {
		server, err := Open(config)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := server.Identity(t.Context())
		if err != nil || identity.Epoch != 0 || !identity.Fenced {
			t.Fatalf("initial identity=%+v err=%v", identity, err)
		}
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenValidatesAttemptEpochBeforeRestoringWait(t *testing.T) {
	for _, epoch := range []uint64{1, 2} {
		t.Run(fmt.Sprint(epoch), func(t *testing.T) {
			config := Config{Directory: t.TempDir()}
			observation := Observation{AttemptID: "original", Epoch: epoch, RunnerID: "prior-runner", State: Running, PID: 123}
			original, err := json.Marshal(journal{Version: journalVersion, Epoch: 1, Attempts: map[string]storedAttempt{
				observation.AttemptID: {Observation: observation, RequestDigest: value.Hash(nil)},
			}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(config.Directory, journalFilename)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			server, err := Open(config)
			if epoch > 1 {
				if server != nil {
					_ = server.Close()
				}
				if err == nil {
					t.Fatal("journal accepted an attempt beyond its creation epoch")
				}
				if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, original) {
					t.Fatalf("refused attempt was rewritten: %q %v", current, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			restored, err := server.Inspect(t.Context(), observation.AttemptID)
			if err != nil || restored.Reference() != observation.Reference() || restored.State != Unknown || restored.PID != 0 || restored.ProofID != "" {
				t.Fatalf("restored observation invented an exit or changed identity: %+v %v", restored, err)
			}
		})
	}
}
