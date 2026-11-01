package browser

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
)

// Prove acknowledges an actual original exit only after its evidence and every
// affected durable protection have settled. A transport retry performs no new
// cleanup and cannot relabel another Realm, Use or scope generation.
func (b *Bridge) Prove(ctx context.Context, token string, proof CloseProof) (ProofAcknowledgement, error) {
	session, err := b.session(token, false)
	if err != nil {
		return ProofAcknowledgement{}, err
	}
	if proof.Realm != session.realm || proof.Authority.ID != b.authority.ID || proof.CloseID == "" || proof.ProofID == "" || proof.Status != Released {
		return ProofAcknowledgement{}, frame.ErrInvalid
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return ProofAcknowledgement{}, err
	}
	if int64(len(encoded)) > b.config.MaxRequestBytes {
		return ProofAcknowledgement{}, frame.ErrCapacity
	}
	attempt, err := b.provenAttempt(ctx, session, proof)
	if err != nil {
		return ProofAcknowledgement{}, err
	}
	scope, publication, err := b.bindProofScope(ctx, session, attempt, proof)
	if err != nil {
		return ProofAcknowledgement{}, err
	}
	prior, err := b.config.Store.ExitProof(ctx, attempt.ID, proof.ProofID)
	if err == nil && !bytes.Equal(prior, encoded) {
		return ProofAcknowledgement{}, sqlite.ErrConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ProofAcknowledgement{}, err
	}
	if err := b.config.Store.RecordExitProof(ctx, attempt.ID, scope, proof.ProofID, encoded); err != nil {
		return ProofAcknowledgement{}, err
	}
	exit := sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: proof.CloseID, Kind: "browser-" + string(proof.ScopeKind), Evidence: encoded}
	if err := b.releaseProven(ctx, proof, attempt, publication, exit); err != nil {
		return ProofAcknowledgement{}, err
	}
	return ProofAcknowledgement{CloseID: proof.CloseID, ProofID: proof.ProofID, Recorded: true, Authority: b.authority}, nil
}

func (b *Bridge) releaseProven(ctx context.Context, proof CloseProof, attempt sqlite.Attempt, publication *controlOperation, exit sqlite.ExitProof) error {
	switch proof.ScopeKind {
	case UseScope:
		return b.releaseUseTree(ctx, proof.UseID, attempt, exit)
	case ExecutionScope:
		return b.releaseExecution(ctx, attempt, exit)
	case PublicationScope:
		if publication != nil {
			b.mu.Lock()
			publication.released = true
			b.mu.Unlock()
		}
	}
	return nil
}

// provenAttempt finds the browser attempt a proof closes and refuses a proof
// minted for an earlier epoch, generation or scope of it.
func (b *Bridge) provenAttempt(ctx context.Context, session *realmSession, proof CloseProof) (sqlite.Attempt, error) {
	records, err := b.config.Store.Attempts(ctx)
	if err != nil {
		return sqlite.Attempt{}, err
	}
	index := slices.IndexFunc(records, func(record sqlite.AttemptRecord) bool { return record.Attempt.ID == proof.Execution.ID })
	if index < 0 {
		return sqlite.Attempt{}, frame.ErrUnavailable
	}
	attempt := records[index].Attempt
	if attempt.Profile != frame.BrowserJS || attempt.Realm != string(RealmID(session.realm)) || proof.Authority.Epoch != strconv.FormatUint(attempt.Epoch, 10) || proof.Execution.Generation != strconv.FormatUint(attempt.Generation, 10) || proof.ScopeID != string(attempt.Scope) || proof.ScopeGeneration != strconv.FormatUint(attempt.ScopeGeneration, 10) {
		return sqlite.Attempt{}, frame.ErrStaleBinding
	}
	if hasDuplicate(proof.Instances) {
		return sqlite.Attempt{}, frame.ErrInvalid
	}
	return attempt, nil
}

// bindProofScope checks that the proof's scope belongs to attempt and returns
// the durable scope key it is recorded under.
func (b *Bridge) bindProofScope(ctx context.Context, session *realmSession, attempt sqlite.Attempt, proof CloseProof) (string, *controlOperation, error) {
	scope := string(proof.ScopeKind)
	switch proof.ScopeKind {
	case ExecutionScope:
		if proof.UseID != "" || proof.PublicationID != "" || len(proof.Instances) != 1 || proof.Instances[0] != attempt.ID {
			return "", nil, frame.ErrInvalid
		}
		return scope, nil, nil
	case UseScope:
		if proof.UseID == "" || proof.PublicationID != "" {
			return "", nil, frame.ErrInvalid
		}
		if err := b.bindProofUse(ctx, session, attempt, proof); err != nil {
			return "", nil, err
		}
		return scope + ":" + proof.UseID, nil, nil
	case PublicationScope:
		if proof.PublicationID == "" || proof.UseID != "" || len(proof.Instances) != 1 || proof.Instances[0] != attempt.ID {
			return "", nil, frame.ErrInvalid
		}
		publication, err := b.bindProofPublication(session, attempt, proof)
		if err != nil {
			return "", nil, err
		}
		return scope + ":" + proof.PublicationID, publication, nil
	default:
		return "", nil, frame.ErrInvalid
	}
}

func (b *Bridge) bindProofUse(ctx context.Context, session *realmSession, attempt sqlite.Attempt, proof CloseProof) error {
	uses, err := b.config.Store.Uses(ctx)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(uses, func(use sqlite.UseRecord) bool { return use.Approval.ID == proof.UseID })
	if index < 0 {
		return frame.ErrStaleBinding
	}
	approval := uses[index].Approval
	if approval.ExecutionID != attempt.ID || approval.Realm != RealmID(session.realm) || approval.Epoch != attempt.Epoch || approval.Scope != attempt.Scope || approval.ScopeGeneration != attempt.ScopeGeneration || !sameSet(approval.Instances, proof.Instances) {
		return frame.ErrStaleBinding
	}
	return nil
}

func (b *Bridge) bindProofPublication(session *realmSession, attempt sqlite.Attempt, proof CloseProof) (*controlOperation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var publication *controlOperation
	for _, candidate := range b.operations {
		if candidate.kind == "publish" && candidate.execution == attempt.ID && candidate.publication.Publication != nil && candidate.publication.Publication.ID == proof.PublicationID {
			publication = candidate
			break
		}
	}
	live := b.executions[attempt.ID] != nil
	valid := publication != nil && publication.realm == session.realm && publication.withdrawn
	if live && !valid {
		return nil, frame.ErrStaleBinding
	}
	// After Host reconstruction the old public catalog is sealed. A
	// subordinate object proof records evidence but releases no attempt or
	// delegated Use, so it cannot widen the original execution authority.
	return publication, nil
}

// A Use's actual exit covers the child range delegated through that original
// owner. Join in-flight child admissions before taking the durable closure; an
// unknown child response cannot make a parent proof wait on itself forever.
func (b *Bridge) releaseUseTree(ctx context.Context, id string, attempt sqlite.Attempt, proof sqlite.ExitProof) error {
	covered := map[string]bool{id: true}
	waited := map[*controlOperation]bool{}
	var records []sqlite.UseRecord
	for {
		var err error
		records, err = b.config.Store.Uses(ctx)
		if err != nil {
			return err
		}
		changed := true
		for changed {
			changed = false
			for _, record := range records {
				if covered[record.Approval.ParentID] && !covered[record.Approval.ID] {
					if record.Approval.ExecutionID != attempt.ID || record.Approval.Realm != frame.RealmID(attempt.Realm) {
						return frame.ErrStaleBinding
					}
					covered[record.Approval.ID] = true
					changed = true
				}
			}
		}
		b.mu.Lock()
		for useID := range covered {
			if live := b.uses[useID]; live != nil {
				live.releaseID = proof.ExitID
			}
		}
		var pending []*controlOperation
		for _, operation := range b.operations {
			if covered[operation.parentID] && !waited[operation] {
				pending = append(pending, operation)
				waited[operation] = true
			}
		}
		b.mu.Unlock()
		if len(pending) == 0 {
			break
		}
		for _, operation := range pending {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-operation.done:
			}
		}
	}
	b.mu.Lock()
	var resources []*frame.ResourceHandle
	for useID := range covered {
		if live := b.uses[useID]; live != nil {
			resources = append(resources, live.resource)
		}
	}
	b.mu.Unlock()
	for _, resource := range resources {
		if err := resource.ConfirmReleased(nil); err != nil {
			return err
		}
	}
	for _, record := range records {
		if covered[record.Approval.ID] {
			if err := b.releaseUse(ctx, record.Approval.ID, attempt, proof, record.Released); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Bridge) releaseUse(ctx context.Context, id string, attempt sqlite.Attempt, proof sqlite.ExitProof, alreadyReleased bool) error {
	b.mu.Lock()
	retained := b.uses[id]
	if retained != nil {
		retained.releaseID = proof.ExitID
	}
	b.mu.Unlock()
	if retained != nil {
		if err := retained.resource.ConfirmReleased(nil); err != nil {
			return err
		}
		outcome, err := retained.use.Close(ctx)
		if !outcome.Released {
			return errors.Join(frame.ErrUnconfirmedRelease, err)
		}
		// Released cleanup diagnostics are retained by the original Close. They
		// do not turn confirmed release back into uncertainty.
		return nil
	}
	if alreadyReleased {
		return nil
	}
	if attempt.Epoch == b.config.Store.Epoch() {
		return frame.ErrUnconfirmedRelease
	}
	return b.config.Store.ReleaseRealmUse(ctx, id, frame.RealmID(attempt.Realm), attempt.ID, proof)
}

func (b *Bridge) releaseExecution(ctx context.Context, attempt sqlite.Attempt, proof sqlite.ExitProof) error {
	b.mu.Lock()
	entry := b.executions[attempt.ID]
	if entry != nil {
		entry.released = true
	}
	var pending []<-chan struct{}
	for _, operation := range b.operations {
		if operation.execution == attempt.ID {
			pending = append(pending, operation.done)
		}
	}
	b.mu.Unlock()
	// A request that crossed the boundary before this exact exit may still be
	// registering its original Use. Join it before reading the durable set.
	for _, done := range pending {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
	}
	if entry != nil {
		if err := b.config.Host.RetireInstance(ctx, attempt.ID); err != nil && !errors.Is(err, frame.ErrUnavailable) {
			return err
		}
	}
	uses, err := b.config.Store.Uses(ctx)
	if err != nil {
		return err
	}
	// Confirm every actual remote resource first. Parent Use retirement can
	// then join its children without depending on the order of this query.
	b.mu.Lock()
	var resources []*frame.ResourceHandle
	for _, record := range uses {
		if record.Approval.ExecutionID == attempt.ID && record.Approval.Realm == frame.RealmID(attempt.Realm) {
			if live := b.uses[record.Approval.ID]; live != nil {
				live.releaseID = proof.ExitID
				resources = append(resources, live.resource)
			}
		}
	}
	b.mu.Unlock()
	for _, resource := range resources {
		if err := resource.ConfirmReleased(nil); err != nil {
			return err
		}
	}
	for _, record := range uses {
		if record.Approval.ExecutionID != attempt.ID || record.Approval.Realm != frame.RealmID(attempt.Realm) {
			continue
		}
		if err := b.releaseUse(ctx, record.Approval.ID, attempt, proof, record.Released); err != nil {
			return err
		}
	}
	if err := b.config.Store.ReleaseAttempt(ctx, attempt.ID, proof); err != nil {
		return err
	}
	if entry != nil {
		if err := entry.resource.ConfirmReleased(nil); err != nil {
			return err
		}
	}
	b.mu.Lock()
	for id, use := range b.uses {
		if use.grant.Execution.ID == attempt.ID {
			delete(b.uses, id)
		}
	}
	for id, route := range b.routes {
		if route.grant.Use.Execution.ID == attempt.ID {
			delete(b.routes, id)
		}
	}
	for id, operation := range b.operations {
		if operation.execution == attempt.ID {
			delete(b.operations, id)
		}
	}
	delete(b.executions, attempt.ID)
	if session := b.sessions[attempt.Realm]; session != nil {
		session.revision++
	}
	b.mu.Unlock()
	return nil
}

func (b *Bridge) Reconnect(ctx context.Context, token string, request ReconnectRequest) (ReconnectResult, error) {
	session, err := b.session(token, false)
	if err != nil {
		return ReconnectResult{}, err
	}
	if request.Realm != session.realm || request.Authority.ID != b.authority.ID || len(request.Executions) > b.config.MaxPending || len(request.PendingProofs) > b.config.MaxPending {
		return ReconnectResult{}, frame.ErrInvalid
	}
	result := ReconnectResult{Authority: b.authority, Realm: session.realm, Mode: Revalidated, AcceptedProofs: []string{}, Executions: []RevalidatedExecution{}}
	for _, proof := range request.PendingProofs {
		ack, err := b.Prove(ctx, token, proof)
		if err != nil {
			return ReconnectResult{}, err
		}
		if ack.Recorded {
			result.AcceptedProofs = append(result.AcceptedProofs, ack.ProofID)
		}
	}
	b.mu.Lock()
	cleanupOnly := session.cleanupOnly || request.Authority != b.authority
	entries := make([]*browserExecution, 0, len(request.Executions))
	for _, execution := range request.Executions {
		entry := b.executions[execution.ID]
		if entry == nil || entry.plan.Execution != execution || entry.plan.Realm != session.realm || entry.released {
			cleanupOnly = true
			continue
		}
		entries = append(entries, entry)
	}
	b.mu.Unlock()
	if !cleanupOnly {
		for _, entry := range entries {
			if err := b.ValidatePlan(ctx, token, entry.plan); err != nil {
				cleanupOnly = true
				break
			}
		}
	}
	if cleanupOnly {
		b.mu.Lock()
		b.sealSessionLocked(session)
		b.mu.Unlock()
		for _, entry := range entries {
			b.fence(entry)
		}
		result.Mode = CleanupOnly
		return result, nil
	}
	for _, entry := range entries {
		result.Executions = append(result.Executions, RevalidatedExecution{Execution: entry.plan.Execution, ScopeID: entry.plan.ScopeID, ScopeGeneration: entry.plan.ScopeGeneration})
	}
	return result, nil
}

func hasDuplicate(values []string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}
func sameSet(left, right []string) bool {
	a, b := slices.Clone(left), slices.Clone(right)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// SealRealm revokes new creation and public admission in this exact Realm.
// Already exposed plans remain protected until their original exit is proven.
// Other windows using the same installation retain their independent scopes.
func (b *Bridge) SealRealm(ctx context.Context, token string) error {
	session, err := b.session(token, false)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.sealSessionLocked(session)
	var entries []*browserExecution
	for _, entry := range b.executions {
		if entry.plan.Realm == session.realm && !entry.released {
			entries = append(entries, entry)
		}
	}
	b.mu.Unlock()
	var failures []error
	for _, entry := range entries {
		b.fence(entry)
		if err := b.config.Host.RetireInstance(ctx, entry.attempt.ID); err != nil && !errors.Is(err, frame.ErrUnavailable) {
			failures = append(failures, err)
			continue
		}
		b.mu.Lock()
		unseen := !entry.exposed
		if unseen {
			entry.released = true
		}
		b.mu.Unlock()
		if !unseen {
			continue
		}
		proof := sqlite.ExitProof{AttemptID: entry.attempt.ID, Realm: entry.attempt.Realm, ExitID: entry.attempt.ID + ":not-delivered", Kind: "not-delivered", Evidence: []byte(`{"delivered":false}`)}
		if err := b.config.Store.ReleaseAttempt(ctx, entry.attempt.ID, proof); err != nil {
			failures = append(failures, err)
			continue
		}
		_ = entry.resource.ConfirmReleased(nil)
		select {
		case entry.ready <- preparedResult{err: frame.ErrFenced}:
		default:
		}
	}
	return errors.Join(failures...)
}

func (b *Bridge) sealSessionLocked(session *realmSession) {
	session.cleanupOnly = true
	session.revision++
	for id, snapshot := range b.snapshots {
		if snapshot.realm == string(RealmID(session.realm)) {
			delete(b.snapshots, id)
		}
	}
}
