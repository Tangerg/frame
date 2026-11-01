package runner

import (
	"errors"
	"testing"
)

func TestAbsentProofRequiresTheOriginalDurableFence(t *testing.T) {
	_, client, ctx := serveFixture(t)
	if err := client.Fence(ctx, 1); err != nil {
		t.Fatal(err)
	}
	identity, err := client.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := AbsenceRequest{AttemptID: "approved-not-dispatched", Epoch: 1, RunnerID: identity.ID}
	if _, err := client.ProveAbsent(ctx, request); !errors.Is(err, ErrUnknown) {
		t.Fatalf("absence in an open creation epoch became proof: %v", err)
	}
	if err := client.Fence(ctx, 2); err != nil {
		t.Fatal(err)
	}
	proof, err := client.ProveAbsent(ctx, request)
	if err != nil || proof.Validate(request) != nil || proof.FenceEpoch != 2 {
		t.Fatalf("sealed original absence: %+v %v", proof, err)
	}
	replayed, err := client.ProveAbsent(ctx, request)
	if err != nil || replayed != proof {
		t.Fatalf("repeated observation changed original evidence: %+v %v", replayed, err)
	}
	wrongOwner := request
	wrongOwner.RunnerID = "another-authority"
	if _, err := client.ProveAbsent(ctx, wrongOwner); !errors.Is(err, ErrUnknown) {
		t.Fatalf("replacement authority was permitted to prove non-creation: %v", err)
	}
	late := launchFixture(t, request.AttemptID, request.Epoch, t.TempDir())
	if _, err := client.Launch(ctx, late); !errors.Is(err, ErrFenced) {
		t.Fatalf("an old launch entered after its absence proof: %v", err)
	}
	current := launchFixture(t, "actually-launched", 2, t.TempDir())
	if _, err := client.Launch(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := client.Fence(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ProveAbsent(ctx, AbsenceRequest{AttemptID: current.AttemptID, Epoch: current.Epoch, RunnerID: identity.ID}); !errors.Is(err, ErrUnknown) {
		t.Fatalf("a recorded creation became non-creation evidence: %v", err)
	}
	if _, err := client.CloseAttempt(ctx, current.AttemptID); err != nil {
		t.Fatal(err)
	}
}
