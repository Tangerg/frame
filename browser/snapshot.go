package browser

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

// The receipt prevents recapture after this lease has been consumed or closed.
// Only the live lease retains candidate provenance; a receipt cannot recreate it.
type snapshotLease struct {
	realm   Realm
	capture *snapshotCapture
}

type snapshotCapture struct {
	original   framehost.Snapshot
	projection Snapshot
	candidates map[string]framehost.CandidateRef
}

func newSnapshotLease(id string, authority Authority, realm Realm, original framehost.Snapshot, points []framehost.ContractRef) (*snapshotLease, error) {
	capture := &snapshotCapture{
		original:   original,
		projection: Snapshot{ID: id, Authority: authority, Points: []PointSnapshot{}},
		candidates: make(map[string]framehost.CandidateRef),
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
			capture.candidates[token] = candidate
			projection.Candidates = append(projection.Candidates, Candidate{PublicationID: candidate.ID(), Source: candidate.Source(), MetadataJSON: string(metadata), Token: token})
		}
		capture.projection.Points = append(capture.projection.Points, projection)
	}
	return &snapshotLease{realm: realm, capture: capture}, nil
}

func (s *snapshotLease) capturedFor(realm Realm) bool {
	return s != nil && s.capture != nil && s.realm == realm
}

func (s *snapshotLease) bind(requests []Selection) (framehost.BindingPlan, error) {
	selections := make([]framehost.SelectionRef, 0, len(requests))
	for _, chosen := range requests {
		candidates := make([]framehost.CandidateRef, 0, len(chosen.Candidates))
		for _, token := range chosen.Candidates {
			candidate, exists := s.capture.candidates[token]
			if !exists || candidate.Source().Contract != chosen.Point {
				return framehost.BindingPlan{}, framehost.ErrStaleBinding
			}
			candidates = append(candidates, candidate)
		}
		selected, err := framehost.SelectRefs(s.capture.original, chosen.Point, chosen.Rule, candidates...)
		if err != nil {
			return framehost.BindingPlan{}, err
		}
		selections = append(selections, selected)
	}
	return framehost.Capture(s.capture.original, selections...)
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
	returned = fmt.Errorf("%w: snapshot capture interrupted", framehost.ErrUnavailable)
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
		return Snapshot{}, framehost.ErrFenced
	}
	b.snapshots[request.OperationID] = lease
	pending = lease
	return copyWire(lease.capture.projection), nil
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
	return lease, b.checkResponse(lease.capture.projection)
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
	if err := operation.snapshotResult(); err != nil {
		return Snapshot{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	lease := b.snapshots[id]
	if !lease.capturedFor(request.Realm) {
		return Snapshot{}, frame.ErrClosed
	}
	return copyWire(lease.capture.projection), nil
}

func (o *controlOperation) ownsSnapshot(realm Realm) error {
	if o.Realm != realm {
		return framehost.ErrFenced
	}
	if o.Kind != snapshotOperation && o.Kind != closeSnapshotOperation {
		return sqlite.ErrConflict
	}
	return nil
}

func (b *Bridge) snapshotPoints(names []string) ([]framehost.ContractRef, error) {
	if len(names) == 0 || hasDuplicate(names) {
		return nil, frame.ErrInvalid
	}
	points := make([]framehost.ContractRef, 0, len(names))
	for _, name := range names {
		var selected framehost.ContractRef
		for _, point := range b.config.Points {
			if point.Name != name {
				continue
			}
			selected = point
			break
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
	if value.IsNil(ctx) {
		return frame.ErrInvalid
	}
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
		operation, replay, err := b.begin(ctx, snapshotID, identity, func(*controlOperation) error {
			return nil
		})
		switch {
		case err != nil:
			return err
		case !replay:
			if err := b.reserveReceipt(ctx, snapshotID, operation); err != nil {
				return errors.Join(err, b.finish(ctx, snapshotID, operation, nil))
			}
			return b.finish(ctx, snapshotID, operation, nil)
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
