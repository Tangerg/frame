package browser

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
)

const (
	defaultMaxRealms       = 64
	defaultMaxPending      = 4096
	defaultMaxMessageBytes = 4 << 20
)

func encodedSize(value any) (int64, error) {
	encoded, err := marshalControl(value)
	return int64(len(encoded)), err
}

func requestSize(method methodName, input any) (int64, error) {
	encoded, err := marshalControl(input)
	if err != nil {
		return 0, err
	}
	return encodedSize(HTTPEnvelope{Method: string(method), Input: encoded})
}

func (b *Bridge) checkRequest(method methodName, input any) error {
	size, err := requestSize(method, input)
	if err != nil {
		return err
	}
	if size > b.config.MaxMessageBytes {
		return frame.ErrCapacity
	}
	return nil
}

func (b *Bridge) checkResponse(output any) error {
	size, err := encodedSize(output)
	if err != nil {
		return err
	}
	if size > b.config.MaxMessageBytes {
		return frame.ErrCapacity
	}
	return nil
}

func (b *Bridge) checkPlanCapacity(plan LocalPlan) error {
	if err := b.checkRequest(admitPlanMethod, plan); err != nil {
		return err
	}
	ready := ReadyReport{OperationID: plan.ApprovalToken, Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution, Objects: []ObjectRef{}, Contributions: []PreparedContribution{}}
	for _, unit := range plan.Units {
		for _, export := range unit.Exports {
			object := ObjectRef{ObjectID: plan.ApprovalToken, Export: LocalExport{InstanceID: unit.Instance.ID, Name: export.Name, Contract: export.Contract}}
			ready.Objects = append(ready.Objects, object)
			if export.Contract.Kind == framehost.PointKind {
				ready.Contributions = append(ready.Contributions, PreparedContribution{Object: object, MetadataJSON: jsonNull})
			}
		}
	}
	if err := b.checkRequest(readyMethod, ready); err != nil {
		return err
	}
	return b.checkExitCapacity(plan, ExecutionScope, "", []string{plan.Execution.ID})
}

func (b *Bridge) checkPublicationCapacity(plan LocalPlan, request PublicationRequest) error {
	identity := maximumEncodedName()
	projection := PublicationResult{Decision: Accepted, Publication: &Publication{ID: identity, OperationID: request.OperationID, Revision: strconv.FormatUint(math.MaxUint64, 10), Object: request.Object}}
	if err := b.checkResponse(projection); err != nil {
		return err
	}
	return b.checkExitCapacity(plan, PublicationScope, identity, []string{plan.Execution.ID})
}

func (b *Bridge) checkRouteCapacity(grant UseGrant, source, receiver LocalExport) error {
	route := RouteGrant{ID: maximumEncodedName(), Use: grant, Source: source, Receiver: receiver}
	return b.checkRequest(validateRouteMethod, route)
}

// A complete pending projection must remain transportable even when every
// admitted execution changes from a plan to a fence before the next poll.
// Reservations also cover the complete reconnect request and its response.
func (b *Bridge) reserveProjectionLocked(session *realmSession, plan LocalPlan) (int64, error) {
	if session.cleanupOnly || b.sessions.get(RealmID(session.realm)) != session {
		return 0, framehost.ErrFenced
	}
	base := Poll{Authority: b.authority, Realm: session.realm, Plans: []LocalPlan{}, Fences: []Fence{}}
	pollBytes, err := encodedSize(base)
	if err != nil {
		return 0, err
	}
	reconnectBytes, err := requestSize(reconnectMethod, ReconnectRequest{Authority: b.authority, Realm: session.realm, Executions: []Execution{}})
	if err != nil {
		return 0, err
	}
	responseBytes, err := encodedSize(ReconnectResult{Authority: b.authority, Realm: session.realm, Mode: Revalidated, Executions: []RevalidatedExecution{}})
	if err != nil {
		return 0, err
	}
	headerBytes := max(pollBytes, reconnectBytes, responseBytes)
	var memberBytes int64
	for _, member := range []any{plan, Fence{ScopeID: plan.ScopeID, ScopeGeneration: plan.ScopeGeneration}, plan.Execution, RevalidatedExecution{Execution: plan.Execution, ScopeID: plan.ScopeID, ScopeGeneration: plan.ScopeGeneration}} {
		size, err := encodedSize(member)
		if err != nil {
			return 0, err
		}
		memberBytes = max(memberBytes, size)
	}
	memberBytes += int64(len(","))
	available := b.config.MaxMessageBytes - headerBytes - session.projectionBytes
	if memberBytes > available {
		return 0, frame.ErrCapacity
	}
	session.projectionBytes += memberBytes
	return memberBytes, nil
}

func (b *Bridge) removeExecutionLocked(entry *browserExecution) {
	if b.executions.get(entry.plan.Execution.ID) != entry {
		return
	}
	b.executions.remove(entry.plan.Execution.ID)
	if session := b.sessions.get(RealmID(entry.plan.Realm)); session != nil {
		session.projectionBytes -= entry.projectionBytes
	}
}

// Exit diagnostics stay on the local Exit. Wire evidence needs only the exact
// original scope, so application failures cannot make cleanup untransportable.
func (b *Bridge) checkExitCapacity(plan LocalPlan, kind CloseScopeKind, id string, instances []string) error {
	identity := maximumEncodedName()
	proof := CloseProof{CloseID: identity, ProofID: identity, Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution, ScopeID: plan.ScopeID, ScopeGeneration: plan.ScopeGeneration, ScopeKind: kind, Instances: instances, Status: frame.ResourceReleased}
	switch kind {
	case UseScope:
		proof.UseID = id
	case PublicationScope:
		proof.PublicationID = id
	}
	if err := b.checkRequest(proveMethod, proof); err != nil {
		return err
	}
	encoded, err := marshalControl(proof)
	if err != nil {
		return err
	}
	if len(encoded) > sqlite.MaxExitEvidenceBytes {
		return fmt.Errorf("%w: exit evidence", frame.ErrCapacity)
	}
	return nil
}

func maximumEncodedName() string {
	// Each permitted identity byte can occupy at most one JSON Unicode escape.
	return strings.Repeat("<", frame.MaxNameBytes)
}
