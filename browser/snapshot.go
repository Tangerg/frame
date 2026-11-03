package browser

import (
	"context"
	"errors"
	"strconv"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
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
			token := newToken()
			lease.candidates[token] = candidate
			projection.Candidates = append(projection.Candidates, Candidate{PublicationID: candidate.ID(), Source: candidate.Source(), MetadataJSON: string(metadata), Token: token})
		}
		lease.projection.Points = append(lease.projection.Points, projection)
	}
	return lease, nil
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
	session, err := b.session(token, false)
	if err != nil {
		return Snapshot{}, err
	}
	if !frame.ValidName(request.OperationID) {
		return Snapshot{}, frame.ErrInvalid
	}
	if err := b.checkRequest(snapshotOperation, request); err != nil {
		return Snapshot{}, err
	}
	digest, err := requestDigest(request)
	if err != nil {
		return Snapshot{}, err
	}
	b.mu.Lock()
	previous, err := b.operationLocked(ctx, request.OperationID)
	if err != nil {
		b.mu.Unlock()
		return Snapshot{}, err
	}
	if previous != nil {
		b.mu.Unlock()
		return b.replaySnapshot(ctx, session.realm, request.OperationID, digest, previous)
	}
	if session.cleanupOnly {
		b.mu.Unlock()
		return Snapshot{}, frame.ErrFenced
	}
	if len(b.snapshots) >= b.config.MaxPending || len(b.operations) >= b.config.MaxPending {
		b.mu.Unlock()
		return Snapshot{}, frame.ErrCapacity
	}
	operation := &controlOperation{kind: snapshotOperation, realm: session.realm, digest: digest, done: make(chan struct{})}
	retained := &snapshotLease{realm: session.realm}
	b.operations[request.OperationID] = operation
	b.snapshots[request.OperationID] = retained
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		operation.failure = returned
		if returned != nil && b.snapshots[request.OperationID] == retained {
			delete(b.snapshots, request.OperationID)
		}
		b.mu.Unlock()
		returned = errors.Join(returned, b.complete(ctx, request.OperationID, operation))
	}()
	if err := b.reserveReceipt(ctx, request.OperationID, operation); err != nil {
		return Snapshot{}, err
	}
	points, err := b.snapshotPoints(request.Points)
	if err != nil {
		return Snapshot{}, err
	}
	original, err := b.config.Host.SnapshotIn(ctx, RealmID(session.realm), points...)
	if err != nil {
		return Snapshot{}, err
	}
	lease, err := newSnapshotLease(request.OperationID, b.authority, session.realm, original, points)
	if err != nil {
		return Snapshot{}, err
	}
	if err := b.checkResponse(lease.projection); err != nil {
		return Snapshot{}, err
	}
	b.mu.Lock()
	if session.cleanupOnly || b.snapshots[request.OperationID] != retained {
		b.mu.Unlock()
		return Snapshot{}, frame.ErrFenced
	}
	b.snapshots[request.OperationID] = lease
	retained = lease
	b.mu.Unlock()
	return clone(lease.projection), nil
}

func (b *Bridge) replaySnapshot(ctx context.Context, realm Realm, id string, digest value.Digest, operation *controlOperation) (Snapshot, error) {
	if operation.realm != realm {
		return Snapshot{}, frame.ErrFenced
	}
	if operation.kind != snapshotOperation && operation.kind != closeSnapshotOperation {
		return Snapshot{}, sqlite.ErrConflict
	}
	if operation.kind == snapshotOperation && operation.digest != digest {
		return Snapshot{}, sqlite.ErrConflict
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
	if lease == nil {
		return Snapshot{}, frame.ErrClosed
	}
	return clone(lease.projection), nil
}

func (b *Bridge) snapshotPoints(names []string) ([]frame.ContractRef, error) {
	if len(names) == 0 || len(names) > b.config.MaxPending || hasDuplicate(names) {
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
	session, err := b.session(token, false)
	if err != nil {
		return err
	}
	if !frame.ValidName(snapshotID) {
		return frame.ErrInvalid
	}
	b.mu.Lock()
	operation, err := b.operationLocked(ctx, snapshotID)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	if operation == nil {
		if session.cleanupOnly {
			b.mu.Unlock()
			return nil
		}
		if len(b.operations) >= b.config.MaxPending {
			b.mu.Unlock()
			return frame.ErrCapacity
		}
		operation = &controlOperation{kind: closeSnapshotOperation, realm: session.realm, failure: frame.ErrClosed, done: make(chan struct{})}
		b.operations[snapshotID] = operation
		b.mu.Unlock()
		if err := b.reserveReceipt(ctx, snapshotID, operation); err != nil {
			operation.failure = err
			return errors.Join(err, b.complete(ctx, snapshotID, operation))
		}
		return b.complete(ctx, snapshotID, operation)
	}
	b.mu.Unlock()
	if operation.realm != session.realm {
		return frame.ErrFenced
	}
	if operation.kind != snapshotOperation && operation.kind != closeSnapshotOperation {
		return sqlite.ErrConflict
	}
	if err := operation.wait(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.snapshots, snapshotID)
	b.mu.Unlock()
	return nil
}
