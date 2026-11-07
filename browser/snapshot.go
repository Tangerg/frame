package browser

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
)

// The receipt prevents recapture after this lease has been consumed or closed.
// Only the live lease retains candidate provenance; a receipt cannot recreate it.
type snapshotLease struct {
	realm      Realm
	original   *frame.Snapshot
	projection Snapshot
	candidates map[string]frame.CandidateRef
}

func newSnapshotLease(id string, authority Authority, realm Realm, original *frame.Snapshot, points []frame.ContractRef) (*snapshotLease, error) {
	lease := &snapshotLease{
		realm: realm, original: original,
		projection: Snapshot{ID: id, Authority: authority, Points: []PointSnapshot{}},
		candidates: make(map[string]frame.CandidateRef),
	}
	for _, point := range points {
		candidates, err := original.Candidates(point)
		if err != nil {
			return nil, err
		}
		revision, err := original.Revision(point)
		if err != nil {
			return nil, err
		}
		projection := PointSnapshot{Point: point, Revision: strconv.FormatUint(revision, 10), Candidates: []Candidate{}}
		for _, candidate := range candidates {
			metadata, err := candidate.MetadataJSON()
			if err != nil {
				return nil, err
			}
			token := rand.Text()
			lease.candidates[token] = candidate
			projection.Candidates = append(projection.Candidates, Candidate{PublicationID: candidate.ID(), Source: candidate.Source(), MetadataJSON: string(metadata), Token: token})
		}
		lease.projection.Points = append(lease.projection.Points, projection)
	}
	return lease, nil
}

func (s *snapshotLease) capturedFor(realm Realm) bool {
	return s != nil && s.original != nil && s.realm == realm
}

func (s *snapshotLease) bind(requests []Selection) (*frame.BindingPlan, error) {
	selections := make([]frame.SelectionRef, 0, len(requests))
	for _, chosen := range requests {
		candidates := make([]frame.CandidateRef, 0, len(chosen.Candidates))
		for _, token := range chosen.Candidates {
			candidate, exists := s.candidates[token]
			if !exists || candidate.Source().Contract != chosen.Point {
				return nil, frame.ErrStaleBinding
			}
			candidates = append(candidates, candidate)
		}
		selected, err := frame.SelectRefs(s.original, chosen.Point, chosen.Rule, candidates...)
		if err != nil {
			return nil, err
		}
		selections = append(selections, selected)
	}
	return frame.Capture(s.original, selections...)
}

func (b *Bridge) Snapshot(ctx context.Context, token string, request SnapshotRequest) (snapshot Snapshot, returned error) {
	session, err := b.anySession(token)
	if err != nil {
		return Snapshot{}, err
	}
	if !frame.ValidName(request.OperationID) {
		return Snapshot{}, frame.ErrInvalid
	}
	if err := b.checkRequest(snapshotMethod, request); err != nil {
		return Snapshot{}, err
	}
	digest, err := requestDigest(request)
	if err != nil {
		return Snapshot{}, err
	}
	identity := controlRequestIdentity{Kind: snapshotOperation, Realm: session.realm, Digest: digest}
	// The placeholder lease reserves capacity and lets a racing close or seal
	// discard the capture before it is published.
	pending := &snapshotLease{realm: session.realm}
	operation, replay, err := b.begin(ctx, request.OperationID, identity, func(*controlOperation) error {
		if session.cleanupOnly {
			return errRealmSealed
		}
		if len(b.snapshots) >= b.config.MaxPending {
			return fmt.Errorf("%w: snapshots", frame.ErrCapacity)
		}
		b.snapshots[request.OperationID] = pending
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	if replay {
		return b.replaySnapshot(ctx, request.OperationID, identity, operation)
	}
	// Metadata codecs may panic or exit this goroutine. Only a completed capture
	// and response copy can replace this failure with a successful receipt.
	returned = fmt.Errorf("%w: snapshot capture interrupted", frame.ErrUnavailable)
	defer func() {
		if returned != nil {
			b.mu.Lock()
			if b.snapshots[request.OperationID] == pending {
				delete(b.snapshots, request.OperationID)
			}
			b.mu.Unlock()
		}
		returned = b.finish(ctx, request.OperationID, operation, returned)
	}()
	if err := b.reserveReceipt(ctx, request.OperationID, operation); err != nil {
		return Snapshot{}, err
	}
	lease, err := b.captureLease(ctx, session.realm, request)
	if err != nil {
		return Snapshot{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if session.cleanupOnly || b.snapshots[request.OperationID] != pending {
		return Snapshot{}, frame.ErrFenced
	}
	b.snapshots[request.OperationID] = lease
	pending = lease
	return copyWire(lease.projection), nil
}

func (b *Bridge) captureLease(ctx context.Context, realm Realm, request SnapshotRequest) (*snapshotLease, error) {
	points, err := b.snapshotPoints(request.Points)
	if err != nil {
		return nil, err
	}
	original, err := b.config.Host.SnapshotIn(ctx, RealmID(realm), points...)
	if err != nil {
		return nil, err
	}
	lease, err := newSnapshotLease(request.OperationID, b.authority, realm, original, points)
	if err != nil {
		return nil, err
	}
	return lease, b.checkResponse(lease.projection)
}

// replaySnapshot observes the original capture of id. A close that already
// claimed id answers every later capture with ErrClosed.
func (b *Bridge) replaySnapshot(ctx context.Context, id string, request controlRequestIdentity, operation *controlOperation) (Snapshot, error) {
	if err := operation.ownsSnapshot(request.Realm); err != nil {
		return Snapshot{}, err
	}
	if operation.Kind == snapshotOperation {
		if err := operation.repeats(request); err != nil {
			return Snapshot{}, err
		}
	}
	if err := operation.wait(ctx); err != nil {
		return Snapshot{}, err
	}
	if operation.failure != nil {
		return Snapshot{}, operation.failure
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	lease := b.snapshots[id]
	if !lease.capturedFor(request.Realm) {
		return Snapshot{}, frame.ErrClosed
	}
	return copyWire(lease.projection), nil
}

func (o *controlOperation) ownsSnapshot(realm Realm) error {
	if o.Realm != realm {
		return frame.ErrFenced
	}
	if o.Kind != snapshotOperation && o.Kind != closeSnapshotOperation {
		return sqlite.ErrConflict
	}
	return nil
}

func (b *Bridge) snapshotPoints(names []string) ([]frame.ContractRef, error) {
	if len(names) == 0 || hasDuplicate(names) {
		return nil, frame.ErrInvalid
	}
	points := make([]frame.ContractRef, 0, len(names))
	for _, name := range names {
		var selected frame.ContractRef
		for _, point := range b.config.Points {
			if point.Name != name {
				continue
			}
			if selected.Name != "" {
				return nil, frame.ErrAmbiguous
			}
			selected = point
		}
		if selected.Name == "" {
			return nil, frame.ErrContractMismatch
		}
		points = append(points, selected)
	}
	return points, nil
}

// A close that arrives before capture reserves the original identity as closed.
// Otherwise a delayed request could create a lease after cleanup was acknowledged.
func (b *Bridge) CloseSnapshot(ctx context.Context, token, snapshotID string) error {
	session, err := b.anySession(token)
	if err != nil {
		return err
	}
	if !frame.ValidName(snapshotID) {
		return frame.ErrInvalid
	}
	if b.sealed(session) {
		// Sealing already discarded every lease of this Realm.
		return nil
	}
	identity := controlRequestIdentity{Kind: closeSnapshotOperation, Realm: session.realm}
	for {
		operation, replay, err := b.begin(ctx, snapshotID, identity, func(operation *controlOperation) error {
			operation.failure = frame.ErrClosed
			return nil
		})
		switch {
		case err != nil:
			return err
		case !replay:
			if err := b.reserveReceipt(ctx, snapshotID, operation); err != nil {
				return errors.Join(err, b.complete(ctx, snapshotID, operation))
			}
			return b.complete(ctx, snapshotID, operation)
		}
		if err := operation.ownsSnapshot(session.realm); err != nil {
			return err
		}
		if err := operation.wait(ctx); err != nil {
			return err
		}
		b.mu.Lock()
		original, err := b.operationLocked(ctx, snapshotID)
		if err != nil {
			b.mu.Unlock()
			return err
		}
		if original == nil {
			b.mu.Unlock()
			// A joined capture may have rolled back its reservation. Close must
			// claim that now-unowned identity before acknowledging terminal cleanup.
			continue
		}
		if err := original.ownsSnapshot(session.realm); err != nil {
			b.mu.Unlock()
			return err
		}
		select {
		case <-original.done:
			delete(b.snapshots, snapshotID)
			b.mu.Unlock()
			return nil
		default:
			b.mu.Unlock()
		}
	}
}
