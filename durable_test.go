package frame_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
)

type resultStore struct {
	state   frame.CommitState
	confirm error
}

func (s resultStore) CommitInstallation(context.Context, frame.InstallationChange) (frame.CommitState, error) {
	return s.state, nil
}
func (s resultStore) ConfirmInstallation(context.Context, frame.InstallationID) error {
	return s.confirm
}
func (s resultStore) Installations(context.Context) ([]frame.InstallationRecord, error) {
	return nil, nil
}

func TestInstallationReportsTheOriginalCommit(t *testing.T) {
	plugin, err := frame.Define(frame.Definition{Revision: integrity.Hash([]byte("test")), Units: []frame.Unit{{Declaration: frame.UnitDeclaration{ID: "sample", Profile: frame.NativeGo}, Setup: func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return nil, nil }}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []frame.CommitState{frame.CommitRejected, frame.CommitIndeterminate, frame.CommitSucceeded} {
		t.Run(string(state), func(t *testing.T) {
			host, err := frame.NewHost(t.Context(), frame.HostConfig{Installations: resultStore{state, errors.New("confirmation unavailable")}})
			if err != nil {
				t.Fatal(err)
			}
			record, err := host.Install(t.Context(), frame.InstallationConfig{ID: "sample", Plugin: plugin})
			if err == nil || frame.InstallationConclusion(err) != state {
				t.Fatalf("state %s: %v", state, err)
			}
			if (record.ID != "") != (state == frame.CommitSucceeded) {
				t.Fatal("only committed records may be returned")
			}
		})
	}
}
