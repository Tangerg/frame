package browser

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/sqlite"
)

// Prove acknowledges an actual original exit only after its evidence and every
// affected durable protection have settled. A transport retry performs no new
// cleanup and cannot relabel another Realm, Use or scope generation.
func (b *Bridge) Prove(ctx context.Context, token string, proof CloseProof) (ProofAcknowledgement, error) {
	session, err := b.anySession(token)
	if err != nil {
		return ProofAcknowledgement{}, err
	}
	if !proof.claims(session.realm, b.authority) {
		return ProofAcknowledgement{}, frame.ErrInvalid
	}
	encoded, err := marshalControl(proof)
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
	exit := proof.exitProof(encoded)
	if err := b.releaseProven(ctx, proof, attempt, publication, exit); err != nil {
		return ProofAcknowledgement{}, err
	}
	return ack, nil
}

// proofKindPrefix qualifies a browser exit proof by the scope it releases.
const proofKindPrefix = "browser-"

// claims reports whether the proof is a well-formed confirmed exit addressed to
// this Realm and authority epoch. Only released evidence may settle protection.
func (p CloseProof) claims(realm Realm, authority Authority) bool {
	return p.Realm == realm && p.Authority.ID == authority.ID && frame.ValidName(p.CloseID) && frame.ValidName(p.ProofID) &&
		p.Status == frame.ResourceReleased && !hasDuplicate(p.Instances)
}

func (p CloseProof) exitKind() string { return proofKindPrefix + string(p.ScopeKind) }

func (p CloseProof) exitProof(encoded []byte) sqlite.ExitProof {
	return sqlite.ExitProof{AttemptID: p.Execution.ID, Realm: string(RealmID(p.Realm)), ExitID: p.CloseID, Kind: p.exitKind(), Evidence: encoded}
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
	entry := b.executions.get(proof.Execution.ID)
	unadmitted := entry != nil && !entry.runtimeOwnsExit()
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
	executing := b.executions.get(attempt.ID) != nil
	b.mu.Unlock()
	if publication != nil {
		if publication.execution != attempt.ID || publication.Realm != session.realm {
			return nil, frame.ErrStaleBinding
		}
		result, err := publication.publicationResult()
		if err != nil || result.Publication == nil {
			return nil, errors.Join(frame.ErrStaleBinding, err)
		}
		if _, err := b.config.Host.Publication(publication.execution, result.Publication.OperationID); !errors.Is(err, frame.ErrUnavailable) {
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
	joined := make(map[*controlOperation]bool)
	for {
		b.mu.Lock()
		delegations := make([]useDelegation, 0, len(b.uses))
		for _, live := range b.uses {
			grant := live.grant
			delegations = append(delegations, useDelegation{grant.ID, grant.ParentUseID, grant.Execution.ID, RealmID(grant.Realm)})
		}
		err := coverDelegated(covered, delegations, attempt)
		if err != nil {
			b.mu.Unlock()
			return err
		}
		// A child can finish and leave the operation journal while SQLite is read.
		// Fence its live owner first, then join admissions before taking that read.
		b.markReleasedLocked(covered, proof.ExitID)
		pending := b.unjoinedLocked(joined, func(operation *controlOperation) bool { return covered[operation.parentID] })
		b.mu.Unlock()
		if len(pending) == 0 {
			break
		}
		if err := awaitOperations(ctx, pending); err != nil {
			return err
		}
	}
	records, err := b.config.Store.ExecutionUses(ctx, attempt.ID)
	if err != nil {
		return err
	}
	delegations := make([]useDelegation, 0, len(records))
	for _, record := range records {
		approval := record.Approval
		delegations = append(delegations, useDelegation{approval.ID, approval.ParentID, approval.ExecutionID, approval.Realm})
	}
	if err := coverDelegated(covered, delegations, attempt); err != nil {
		return err
	}
	return b.releaseCovered(ctx, covered, records, attempt, proof)
}

type useDelegation struct {
	id, parentID, executionID string
	realm                     frame.RealmID
}

func coverDelegated(covered map[string]bool, delegations []useDelegation, attempt sqlite.Attempt) error {
	for changed := true; changed; {
		changed = false
		for _, delegation := range delegations {
			if !covered[delegation.parentID] || covered[delegation.id] {
				continue
			}
			if delegation.executionID != attempt.ID || delegation.realm != frame.RealmID(attempt.Realm) {
				return frame.ErrStaleBinding
			}
			covered[delegation.id] = true
			changed = true
		}
	}
	return nil
}

func (b *Bridge) markReleasedLocked(uses map[string]bool, exitID string) {
	for id := range uses {
		if live := b.uses[id]; live != nil {
			live.releasedBy = exitID
		}
	}
}

// unjoinedLocked returns the live operations selected by belongs that have not
// been joined yet, and marks them joined.
func (b *Bridge) unjoinedLocked(joined map[*controlOperation]bool, belongs func(*controlOperation) bool) []*controlOperation {
	var pending []*controlOperation
	for _, operation := range b.operations {
		if belongs(operation) && !joined[operation] {
			joined[operation] = true
			pending = append(pending, operation)
		}
	}
	return pending
}

func awaitOperations(ctx context.Context, operations []*controlOperation) error {
	for _, operation := range operations {
		if err := operation.wait(ctx); err != nil {
			return err
		}
	}
	return nil
}

// releaseCovered confirms every live resource first, so a parent Use can then
// join its children regardless of the order of the durable records.
func (b *Bridge) releaseCovered(ctx context.Context, covered map[string]bool, records []sqlite.UseRecord, attempt sqlite.Attempt, proof sqlite.ExitProof) error {
	b.mu.Lock()
	var resources []*frame.ResourceHandle
	for id := range covered {
		if live := b.uses[id]; live != nil {
			resources = append(resources, live.resource)
		}
	}
	b.mu.Unlock()
	if err := confirmAll(resources); err != nil {
		return err
	}
	for _, record := range records {
		if covered[record.Approval.ID] {
			if err := b.releaseUse(ctx, record, attempt, proof); err != nil {
				return err
			}
		}
	}
	return nil
}

func confirmAll(resources []*frame.ResourceHandle) error {
	for _, resource := range resources {
		if err := resource.ConfirmReleased(nil); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bridge) releaseUse(ctx context.Context, record sqlite.UseRecord, attempt sqlite.Attempt, proof sqlite.ExitProof) error {
	id := record.Approval.ID
	b.mu.Lock()
	retained := b.uses[id]
	if retained != nil {
		retained.releasedBy = proof.ExitID
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
		for routeID, route := range b.routes.all() {
			if route.grant.Use.ID == id {
				b.routes.remove(routeID)
			}
		}
		b.mu.Unlock()
		// Released cleanup diagnostics are retained by the original Close. They
		// do not turn confirmed release back into uncertainty.
		return nil
	}
	if record.Released {
		return nil
	}
	if attempt.Epoch == b.config.Store.Epoch() {
		return frame.ErrUnconfirmedRelease
	}
	return b.config.Store.ReleaseRealmUse(ctx, id, frame.RealmID(attempt.Realm), attempt.ID, proof)
}

// releaseExecution settles an admitted execution from its exit proof: its
// in-flight operations, its Host instance, every Use it served and finally
// its durable attempt.
func (b *Bridge) releaseExecution(ctx context.Context, attempt sqlite.Attempt, proof sqlite.ExitProof) error {
	b.mu.Lock()
	entry := b.executions.get(attempt.ID)
	if entry != nil {
		entry.fence()
	}
	pending := b.unjoinedLocked(make(map[*controlOperation]bool), func(operation *controlOperation) bool { return operation.execution == attempt.ID })
	b.mu.Unlock()
	// A request that crossed the boundary before this exact exit may still be
	// registering its original Use. Join it before reading the durable set.
	if err := awaitOperations(ctx, pending); err != nil {
		return err
	}
	if entry != nil {
		if err := b.config.Host.RetireInstance(ctx, attempt.ID); err != nil && !errors.Is(err, frame.ErrUnavailable) {
			return err
		}
	}
	records, err := b.config.Store.ExecutionUses(ctx, attempt.ID)
	if err != nil {
		return err
	}
	served := make(map[string]bool, len(records))
	for _, record := range records {
		if record.Approval.ExecutionID == attempt.ID && record.Approval.Realm == frame.RealmID(attempt.Realm) {
			served[record.Approval.ID] = true
		}
	}
	b.mu.Lock()
	b.markReleasedLocked(served, proof.ExitID)
	b.mu.Unlock()
	if err := b.releaseCovered(ctx, served, records, attempt, proof); err != nil {
		return err
	}
	if err := b.config.Store.ReleaseAttempt(ctx, attempt.ID, proof); err != nil {
		return err
	}
	if entry != nil {
		if err := entry.resourceHandle().ConfirmReleased(nil); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.forgetExecutionLocked(attempt, entry)
	b.mu.Unlock()
	return nil
}

func (b *Bridge) forgetExecutionLocked(attempt sqlite.Attempt, entry *browserExecution) {
	for id, use := range b.uses {
		if use.grant.Execution.ID == attempt.ID {
			delete(b.uses, id)
		}
	}
	for id, route := range b.routes.all() {
		if route.grant.Use.Execution.ID == attempt.ID {
			b.routes.remove(id)
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
}

// An unavailable validation does not prove that the original authority ended.
// Only an observed stale or closed owner permits sealing the Realm.
func (b *Bridge) revalidate(ctx context.Context, token string, session *realmSession, request ReconnectRequest) ([]*browserExecution, bool, error) {
	if err := cancellation.Err(ctx); err != nil {
		return nil, false, err
	}
	entries, current := b.reportedExecutions(session, request)
	if !current {
		return nil, false, nil
	}
	for _, entry := range entries {
		err := b.validateCurrentPlan(ctx, token, entry.plan)
		switch {
		case err == nil:
		case cancellation.Err(ctx) != nil || errors.Is(err, frame.ErrRecoveryRequired):
			return nil, false, err
		case errors.Is(err, frame.ErrStaleBinding), errors.Is(err, frame.ErrClosed), errors.Is(err, frame.ErrFenced):
			return nil, false, nil
		default:
			return nil, false, err
		}
	}
	return entries, true, nil
}

func (b *Bridge) reportedExecutions(session *realmSession, request ReconnectRequest) ([]*browserExecution, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session.cleanupOnly || request.Authority != b.authority {
		return nil, false
	}
	entries := make([]*browserExecution, 0, len(request.Executions))
	for _, execution := range request.Executions {
		entry := b.executions.get(execution.ID)
		if entry == nil || entry.plan.Execution != execution || entry.plan.Realm != session.realm || entry.isFenced() {
			return nil, false
		}
		entries = append(entries, entry)
	}
	return entries, true
}

func (b *Bridge) Reconnect(ctx context.Context, token string, request ReconnectRequest) (ReconnectResult, error) {
	if err := b.checkRequest(reconnectMethod, request); err != nil {
		return ReconnectResult{}, err
	}
	session, err := b.anySession(token)
	if err != nil {
		return ReconnectResult{}, err
	}
	if request.Realm != session.realm || request.Authority.ID != b.authority.ID || len(request.Executions) > b.config.MaxPending {
		return ReconnectResult{}, frame.ErrInvalid
	}
	result := ReconnectResult{Authority: b.authority, Realm: session.realm, Mode: Revalidated, Executions: []RevalidatedExecution{}}
	entries, revalidated, err := b.revalidate(ctx, token, session, request)
	if err != nil {
		return ReconnectResult{}, err
	}
	if !revalidated {
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
