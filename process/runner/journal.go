package runner

import (
	"crypto/rand"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/Tangerg/frame/internal/localfile"
	framevalue "github.com/Tangerg/frame/value"
)

const (
	journalFilename       = "attempts.json"
	temporaryRecordPrefix = ".record-"
	lostWaitReason        = "runner restarted without the original child wait identity"
)

type storedAttempt struct {
	Observation   Observation       `json:"observation"`
	RequestDigest framevalue.Digest `json:"requestDigest"`
}

type journal struct {
	Epoch    uint64                   `json:"epoch"`
	Attempts map[string]storedAttempt `json:"attempts"`
}

// loadJournal reads the attempts of an earlier runner. A child it was still
// waiting for becomes Unknown: no later runner holds that wait identity.
func loadJournal(directory *os.Root, maxAttempts int) (journal, error) {
	data, err := directory.ReadFile(journalFilename)
	if errors.Is(err, os.ErrNotExist) {
		return journal{Attempts: make(map[string]storedAttempt)}, nil
	}
	if err != nil {
		return journal{}, err
	}
	var loaded journal
	if err := framevalue.DecodeValue(data, &loaded, "runner journal"); err != nil {
		return journal{}, fmt.Errorf("runner: invalid journal: %w", err)
	}
	if loaded.Attempts == nil || len(loaded.Attempts) > maxAttempts {
		return journal{}, errors.New("runner: incompatible journal")
	}
	for id, attempt := range loaded.Attempts {
		if id != attempt.Observation.AttemptID || attempt.Observation.validate() != nil || attempt.Observation.Epoch > loaded.Epoch || attempt.RequestDigest.Validate() != nil {
			return journal{}, errors.New("runner: invalid attempt record")
		}
		switch attempt.Observation.State {
		case Starting, Running:
			attempt.Observation.State = Unknown
			attempt.Observation.Reason = lostWaitReason
			attempt.Observation.PID = 0
			loaded.Attempts[id] = attempt
		case Exited, Unknown:
		default:
			return journal{}, errors.New("runner: invalid attempt state")
		}
	}
	return loaded, nil
}

func (s *Server) persist() error {
	projection := s.journal
	if len(s.terminalObservations) != 0 {
		projection.Attempts = maps.Clone(s.journal.Attempts)
		for id, observation := range s.terminalObservations {
			item := projection.Attempts[id]
			item.Observation = observation
			if observation.State == NotCreated {
				// A refusal must remain replayable even when every active slot is
				// occupied. It uses the same immutable receipt as acknowledged exits.
				if err := s.storeReceipt(item); err != nil {
					return err
				}
				delete(projection.Attempts, id)
			} else {
				projection.Attempts[id] = item
			}
		}
	}
	if err := storeRecord(s.directory, journalFilename, projection); err != nil {
		return err
	}
	s.journal = projection
	clear(s.terminalObservations)
	return nil
}

func storeRecord(directory *os.Root, path string, value any) (err error) {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	name := filepath.Join(filepath.Dir(path), temporaryRecordPrefix+rand.Text())
	file, err := directory.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, localfile.PrivateFileMode)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := directory.Remove(name); !errors.Is(cleanupErr, os.ErrNotExist) {
			err = errors.Join(err, cleanupErr)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := directory.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(directory, filepath.Dir(path))
}

func syncDirectory(root *os.Root, path string) error {
	directory, err := root.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
