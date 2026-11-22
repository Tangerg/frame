package host

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/Tangerg/frame"
)

// Snapshot is a consistent read of the catalog for selected contracts. Its
// members are projections; Capture rechecks each against the live catalog.
// Copies retain the original read and candidate provenance.
type Snapshot struct{ *catalogSnapshot }

// Valid reports whether the handle names a catalog read; the zero Snapshot does not.
func (s Snapshot) Valid() bool { return s.catalogSnapshot != nil }

type catalogSnapshot struct {
	owner   *frame.LifetimeDomain
	reads   []pointRead
	members map[ContractRef][]snapshotMember
	realm   RealmID
}

type pointRead struct {
	ref      ContractRef
	revision uint64
}

type snapshotMember struct {
	id                  string
	source              ExportRef
	metadata            publicationMetadata
	instanceID          string
	binding             BindingSnapshot
	reconstructionError error
}

// Candidate is one snapshot member with its metadata decoded for this read, so
// a selection rule reads metadata as data. Every Candidates call decodes
// afresh: a rule may reorder or change these values without affecting another
// read of the same publication.
type Candidate[M, V any] struct {
	CandidateRef
	Metadata M
}

// CandidateRef names one member of the original snapshot read; selection
// accepts it only together with that same read.
type CandidateRef struct {
	snapshot *catalogSnapshot
	member   snapshotMember
}

func (c CandidateRef) ID() string         { return c.member.id }
func (c CandidateRef) Source() ExportRef  { return c.member.source }
func (c CandidateRef) InstanceID() string { return c.member.instanceID }
func (c CandidateRef) MetadataJSON() ([]byte, error) {
	return c.member.metadata.wireJSON()
}

type SelectionRef struct {
	id       string
	snapshot *catalogSnapshot
	contract ContractRef
	rule     string
	members  []snapshotMember
}

type Selection[M, V any] struct{ value SelectionRef }

func (s Selection[M, V]) Ref() SelectionRef { return s.value }

func (h *hostState) Snapshot(ctx context.Context, points ...ContractRef) (Snapshot, error) {
	return h.SnapshotIn(ctx, "", points...)
}

func (h *hostState) SnapshotIn(ctx context.Context, realm RealmID, points ...ContractRef) (Snapshot, error) {
	if err := frame.ContextError(ctx); err != nil {
		return Snapshot{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime.IsClosing() {
		return Snapshot{}, frame.ErrClosed
	}
	return h.snapshotLocked(realm, points)
}

// snapshotLocked reads native members only from the requested realm; a
// portable member can be called from any realm.
func (h *hostState) snapshotLocked(realm RealmID, points []ContractRef) (Snapshot, error) {
	result := &catalogSnapshot{owner: h.domain, members: make(map[ContractRef][]snapshotMember, len(points)), realm: realm}
	for _, point := range points {
		if h.contracts.lookup(point) == nil {
			return Snapshot{}, frame.ErrContractMismatch
		}
		if _, duplicate := result.members[point]; duplicate {
			return Snapshot{}, fmt.Errorf("%w: duplicate snapshot contract", frame.ErrInvalid)
		}
		members := []snapshotMember{}
		for _, value := range h.catalog.members(point) {
			if point.Mode == NativeMode && value.instance.realm != realm {
				continue
			}
			members = append(members, value.snapshotIn(realm))
		}
		result.reads = append(result.reads, pointRead{ref: point, revision: h.catalog.revision(point)})
		result.members[point] = members
	}
	return Snapshot{result}, nil
}

func (s *catalogSnapshot) Candidates(contract ContractRef) ([]CandidateRef, error) {
	if s == nil {
		return nil, frame.ErrInvalid
	}
	members, found := s.members[contract]
	if !found {
		return nil, frame.ErrContractMismatch
	}
	result := make([]CandidateRef, len(members))
	for i, member := range members {
		result[i] = CandidateRef{snapshot: s, member: member}
	}
	return result, nil
}

func (s *catalogSnapshot) Revision(contract ContractRef) (uint64, error) {
	if s == nil {
		return 0, frame.ErrInvalid
	}
	for _, read := range s.reads {
		if read.ref == contract {
			return read.revision, nil
		}
	}
	return 0, frame.ErrContractMismatch
}

func Candidates[M, V any](snapshot Snapshot, point Point[M, V]) ([]Candidate[M, V], error) {
	if !snapshot.Valid() || point.value == nil {
		return nil, frame.ErrInvalid
	}
	members, found := snapshot.members[point.Ref()]
	if !found {
		return nil, frame.ErrContractMismatch
	}
	result := make([]Candidate[M, V], len(members))
	for i, member := range members {
		metadata, err := readMetadata[M](member.metadata)
		if err != nil {
			return nil, err
		}
		result[i] = Candidate[M, V]{CandidateRef: CandidateRef{snapshot: snapshot.catalogSnapshot, member: member}, Metadata: metadata}
	}
	return result, nil
}

func SelectRefs(snapshot Snapshot, contract ContractRef, rule string, candidates ...CandidateRef) (SelectionRef, error) {
	if !snapshot.Valid() {
		return SelectionRef{}, frame.ErrInvalid
	}
	if err := validateName("selection rule", rule); err != nil {
		return SelectionRef{}, err
	}
	if _, exists := snapshot.members[contract]; !exists {
		return SelectionRef{}, frame.ErrContractMismatch
	}
	selection := SelectionRef{id: rand.Text(), snapshot: snapshot.catalogSnapshot, contract: contract, rule: rule, members: []snapshotMember{}}
	chosen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		if candidate.snapshot != snapshot.catalogSnapshot || candidate.member.source.Contract != contract {
			return SelectionRef{}, frame.ErrContractMismatch
		}
		if chosen[candidate.member.id] {
			return SelectionRef{}, frame.ErrInvalid
		}
		chosen[candidate.member.id] = true
		selection.members = append(selection.members, candidate.member)
	}
	return selection, nil
}

func Select[M, V any](snapshot Snapshot, point Point[M, V], rule string, candidates ...Candidate[M, V]) (Selection[M, V], error) {
	if point.value == nil {
		return Selection[M, V]{}, frame.ErrInvalid
	}
	refs := make([]CandidateRef, len(candidates))
	for i, candidate := range candidates {
		refs[i] = candidate.CandidateRef
	}
	ref, err := SelectRefs(snapshot, point.Ref(), rule, refs...)
	return Selection[M, V]{value: ref}, err
}

func SelectOne[M, V any](snapshot Snapshot, point Point[M, V], rule string, choose func([]Candidate[M, V]) (Candidate[M, V], error)) (Selection[M, V], error) {
	if choose == nil {
		return Selection[M, V]{}, frame.ErrInvalid
	}
	return SelectMany(snapshot, point, rule, func(candidates []Candidate[M, V]) ([]Candidate[M, V], error) {
		chosen, err := choose(candidates)
		return []Candidate[M, V]{chosen}, err
	})
}

func SelectMany[M, V any](snapshot Snapshot, point Point[M, V], rule string, choose func([]Candidate[M, V]) ([]Candidate[M, V], error)) (Selection[M, V], error) {
	if choose == nil {
		return Selection[M, V]{}, frame.ErrInvalid
	}
	candidates, err := Candidates(snapshot, point)
	if err != nil {
		return Selection[M, V]{}, err
	}
	chosen, err := choose(candidates)
	if err != nil {
		return Selection[M, V]{}, err
	}
	return Select(snapshot, point, rule, chosen...)
}
