package browser

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

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
	if proof.Realm != session.realm || proof.Authority.ID != b.authority.ID || !frame.ValidName(proof.CloseID) || !frame.ValidName(proof.ProofID) || proof.Status != Released {
		return ProofAcknowledgement{}, frame.ErrInvalid
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		return ProofAcknowledgement{}, err
	}
	if err := b.checkRequest(proveMethod, proof); err != nil {
		return ProofAcknowledgement{}, err
	}
	attempt, err := b.provenAttempt(ctx, session, proof)
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
	ack := ProofAcknowledgement{CloseID: proof.CloseID, ProofID: proof.ProofID, Recorded: true, Authority: b.authority}
	if err == nil && proof.ScopeKind == PublicationScope {
		return ack, nil
	}
	scope, publication, err := b.bindProofScope(ctx, session, attempt, proof)
	if err != nil {
		return ProofAcknowledgement{}, err
	}
	if err := b.config.Store.RecordExitProof(ctx, attempt.ID, scope, proof.ProofID, encoded); err != nil {
		return ProofAcknowledgement{}, err
	}
	exit := sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: proof.CloseID, Kind: "browser-" + string(proof.ScopeKind), Evidence: encoded}
	if err := b.releaseProven(ctx, proof, attempt, publication, exit); err != nil {
		return ProofAcknowledgement{}, err
	}
	return ack, nil
}

func (b *Bridge) releaseProven(ctx context.Context, proof CloseProof, attempt sqlite.Attempt, publication *controlOperation, exit sqlite.ExitProof) error {
	switch proof.ScopeKind {
	case UseScope:
		return b.releaseUseTree(ctx, proof.UseID, attempt, exit)
	case ExecutionScope:
		return b.releaseExecution(ctx, attempt, exit)
	case PublicationScope:
		b.mu.Lock()
		if publication != nil && b.publications[proof.PublicationID] == publication {
			delete(b.publications, proof.PublicationID)
		}
		b.mu.Unlock()
	}
	return nil
}

func (b *Bridge) provenAttempt(ctx context.Context, session *realmSession, proof CloseProof) (sqlite.Attempt, error) {
	b.mu.Lock()
	entry := b.executions[proof.Execution.ID]
	unadmitted := entry != nil && !entry.admitted
	b.mu.Unlock()
	if unadmitted {
		return sqlite.Attempt{}, frame.ErrStaleBinding
	}
	record, err := b.config.Store.Attempt(ctx, proof.Execution.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlite.Attempt{}, frame.ErrUnavailable
	}
	if err != nil {
		return sqlite.Attempt{}, err
	}
	attempt := record.Attempt
	if isUnadmittedRecord(record) || !matchesAttempt(attempt, proof.Authority, session.realm, proof.Execution, proof.ScopeID, proof.ScopeGeneration) {
		return sqlite.Attempt{}, frame.ErrStaleBinding
	}
	if hasDuplicate(proof.Instances) {
		return sqlite.Attempt{}, frame.ErrInvalid
	}
	return attempt, nil
}

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
		return scope + ":" + proof.PublicationID, publication, err
	default:
		return "", nil, frame.ErrInvalid
	}
}

func (b *Bridge) bindProofUse(ctx context.Context, session *realmSession, attempt sqlite.Attempt, proof CloseProof) error {
	record, err := b.config.Store.Use(ctx, proof.UseID)
	if errors.Is(err, sql.ErrNoRows) {
		return frame.ErrStaleBinding
	}
	if err != nil {
		return err
	}
	approval := record.Approval
	if approval.ExecutionID != attempt.ID || approval.Realm != RealmID(session.realm) || approval.Epoch != attempt.Epoch || approval.Scope != attempt.Scope || approval.ScopeGeneration != attempt.ScopeGeneration || !sameSet(approval.Instances, proof.Instances) {
		return frame.ErrStaleBinding
	}
	return nil
}

func (b *Bridge) bindProofPublication(session *realmSession, attempt sqlite.Attempt, proof CloseProof) (*controlOperation, error) {
	b.mu.Lock()
	publication := b.publications[proof.PublicationID]
	executing := b.executions[attempt.ID] != nil
	b.mu.Unlock()
	if publication != nil {
		if publication.execution != attempt.ID || publication.realm != session.realm {
			return nil, frame.ErrStaleBinding
		}
		if _, err := b.config.Host.Publication(publication.execution, publication.publication.Publication.OperationID); !errors.Is(err, frame.ErrUnavailable) {
			return nil, errors.Join(frame.ErrStaleBinding, err)
		}
		return publication, nil
	}
	if executing {
		return nil, frame.ErrStaleBinding
	}
	// Reconstruction may retain a subordinate proof after its catalog is gone.
	// Such evidence owns no transition in the current catalog.
	return nil, nil
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
		records, err = b.config.Store.ExecutionUses(ctx, attempt.ID)
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
		// The proof can arrive after an initial unconfirmed close. Join that same
		// exit's terminal evidence before asking the Use to settle durable protection.
		if outcome, err := retained.use.BeginClose(frame.ErrClosed).WaitReleased(ctx); !outcome.Released {
			return errors.Join(frame.ErrUnconfirmedRelease, err)
		}
		outcome, err := retained.use.Close(ctx)
		if !outcome.Released {
			return errors.Join(frame.ErrUnconfirmedRelease, err)
		}
		b.mu.Lock()
		delete(b.uses, id)
		for routeID, route := range b.routes {
			if route.grant.Use.ID == id {
				delete(b.routes, routeID)
			}
		}
		b.mu.Unlock()
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
		entry.retiring = true
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
	uses, err := b.config.Store.ExecutionUses(ctx, attempt.ID)
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
	for id, operation := range b.publications {
		if operation.execution == attempt.ID {
			delete(b.publications, id)
		}
	}
	if entry != nil {
		b.removeExecutionLocked(entry)
		b.reapRealmLocked(entry.plan.Realm)
	}
	if session := b.sessions[attempt.Realm]; session != nil {
		session.revision++
	}
	b.mu.Unlock()
	return nil
}

func (b *Bridge) Reconnect(ctx context.Context, token string, request ReconnectRequest) (ReconnectResult, error) {
	if err := b.checkRequest(reconnectMethod, request); err != nil {
		return ReconnectResult{}, err
	}
	session, err := b.session(token, false)
	if err != nil {
		return ReconnectResult{}, err
	}
	if request.Realm != session.realm || request.Authority.ID != b.authority.ID || len(request.Executions) > b.config.MaxPending {
		return ReconnectResult{}, frame.ErrInvalid
	}
	result := ReconnectResult{Authority: b.authority, Realm: session.realm, Mode: Revalidated, Executions: []RevalidatedExecution{}}
	b.mu.Lock()
	cleanupOnly := session.cleanupOnly || request.Authority != b.authority
	entries := make([]*browserExecution, 0, len(request.Executions))
	for _, execution := range request.Executions {
		entry := b.executions[execution.ID]
		if entry == nil || entry.plan.Execution != execution || entry.plan.Realm != session.realm || entry.retiring {
			cleanupOnly = true
			continue
		}
		entries = append(entries, entry)
	}
	b.mu.Unlock()
	if !cleanupOnly {
		for _, entry := range entries {
			if err := b.validateCurrentPlan(ctx, token, entry.plan); err != nil {
				cleanupOnly = true
				break
			}
		}
	}
	if cleanupOnly {
		if err := b.SealRealm(ctx, token); err != nil {
			return ReconnectResult{}, err
		}
		result.Mode = CleanupOnly
		return result, nil
	}
	for _, entry := range entries {
		result.Executions = append(result.Executions, RevalidatedExecution{Execution: entry.plan.Execution, ScopeID: entry.plan.ScopeID, ScopeGeneration: entry.plan.ScopeGeneration})
	}
	if err := b.checkResponse(result); err != nil {
		return ReconnectResult{}, err
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
// Already admitted plans remain protected until their original exit is proven.
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
		if entry.plan.Realm == session.realm {
			entries = append(entries, entry)
		}
	}
	b.reapRealmLocked(session.realm)
	b.mu.Unlock()
	if session.application != nil {
		session.application.BeginClose(frame.ErrFenced)
	}
	var failures []error
	for _, entry := range entries {
		b.fence(entry)
		if err := b.config.Host.RetireInstance(ctx, entry.attempt.ID); err != nil && !errors.Is(err, frame.ErrUnavailable) {
			failures = append(failures, err)
			continue
		}
		_, err := b.closeExecution(ctx, entry)
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (b *Bridge) sealSessionLocked(session *realmSession) {
	session.cleanupOnly = true
	session.revision++
	for id, snapshot := range b.snapshots {
		if snapshot.realm == session.realm {
			delete(b.snapshots, id)
		}
	}
}
