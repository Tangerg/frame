package browser

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/sqlite"
	framevalue "github.com/Tangerg/frame/value"
)

// emptyConfiguration is the public configuration of a unit whose installation
// projects nothing to the browser.
const emptyConfiguration = `{}`

const executionResourcePrefix = "browser execution "

// Ready rejections tell the Runtime why its report was refused. They name a
// protocol condition and never carry application diagnostics.
const (
	problemWrongAuthority       = "wrong execution authority"
	problemExecutionUnavailable = "execution unavailable"
	problemInvalidObjectSource  = "invalid object source"
	problemDuplicateExport      = "duplicate export name"
	problemDuplicateObject      = "duplicate prepared object"
	problemInexactPoint         = "metadata does not identify an exact prepared point"
	problemDuplicateMetadata    = "duplicate prepared metadata"
	problemUndeclaredExports    = "actual exports differ from approved declarations"
	problemInvalidPointMetadata = "invalid point metadata"
	problemUndeclaredMember     = "undeclared prepared contribution"
	problemMissingMetadata      = "missing prepared metadata"
	problemInvalidMemberData    = "invalid prepared metadata"
	problemExecutionReleased    = "execution already released"
)

func rejected(problem string) PublicationResult {
	return PublicationResult{Decision: Rejected, Problem: problem}
}

func (b *Bridge) Prepare(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
	if !b.config.Host.OwnsSetup(setup) {
		return nil, frame.ErrFenced
	}
	session, err := b.reservePreparation(setup)
	if err != nil {
		return nil, err
	}
	reserved := true
	defer func() {
		if reserved {
			b.mu.Lock()
			b.executions.cancel()
			b.mu.Unlock()
		}
	}()
	plan, err := b.localPlan(ctx, session.realm, setup)
	if err != nil {
		return nil, err
	}
	if err := b.checkPlanCapacity(plan); err != nil {
		return nil, err
	}
	b.mu.Lock()
	projectionBytes, err := b.reserveProjectionLocked(session, plan)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	projectionHeld := true
	defer func() {
		if projectionHeld {
			b.mu.Lock()
			session.projectionBytes -= projectionBytes
			b.reapRealmLocked(session.realm)
			b.mu.Unlock()
		}
	}()
	attempt := sqlite.Attempt{ID: setup.InstanceID(), Epoch: setup.Epoch(), Source: setup.Binding().Source, Binding: setup.Snapshot(), Realm: string(RealmID(session.realm)), Profile: frame.BrowserJS, Scope: setup.ScopeGrant().WireID(), ScopeGeneration: setup.ScopeGrant().Generation(), Generation: setup.Generation()}
	if err := b.config.Store.CheckExitCapacity(unadmittedExitProof(attempt)); err != nil {
		return nil, err
	}
	entry := &browserExecution{plan: plan, attempt: attempt, setup: setup, prepared: make(chan []frame.ExportValue, 1), projectionBytes: projectionBytes}
	registered := make(chan struct{})
	resource, err := setup.Own(frame.Resource{Name: executionResourcePrefix + attempt.ID, Cancel: func(error) { b.fence(entry) }, Close: func(wait context.Context) (frame.ReleaseState, error) {
		select {
		case <-registered:
			return b.closeExecution(wait, entry)
		case <-wait.Done():
			return frame.ResourceUnconfirmed, context.Cause(wait)
		}
	}})
	if err != nil {
		return nil, err
	}
	// Reserve cleanup first: every possibly committed approval keeps a local
	// non-delivery witness, including failures while the database commits.
	approvalErr := b.config.Store.RecordAttempt(ctx, attempt)
	b.mu.Lock()
	entry.recordAttempt(resource, approvalErr)
	reserved = false
	b.executions.commit(plan.Execution.ID, entry)
	projectionHeld = false
	if session.cleanupOnly || ctx.Err() != nil {
		entry.fence()
	}
	revoked := entry.isFenced()
	close(registered)
	b.mu.Unlock()
	if revoked {
		_, err := b.closeExecution(context.WithoutCancel(ctx), entry)
		if approvalErr != nil {
			return nil, errors.Join(approvalErr, err)
		}
		return nil, errors.Join(frame.ErrFenced, err)
	}
	select {
	case exports := <-entry.prepared:
		return exports, nil
	case <-ctx.Done():
		b.fence(entry)
		return nil, cancellation.Err(ctx)
	}
}

// reservePreparation counts a Setup against MaxPending before any I/O.
func (b *Bridge) reservePreparation(setup *frame.Setup) (*realmSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session := b.sessions.get(setup.Realm())
	if session == nil || session.cleanupOnly {
		return nil, frame.ErrFenced
	}
	return session, b.executions.reserve()
}

func (b *Bridge) localPlan(ctx context.Context, realm Realm, setup *frame.Setup) (LocalPlan, error) {
	unit, err := b.plannedUnit(ctx, setup)
	if err != nil {
		return LocalPlan{}, err
	}
	grant := setup.ScopeGrant()
	return LocalPlan{
		Protocol:        Protocol,
		ApprovalToken:   rand.Text(),
		Authority:       b.authority,
		Realm:           realm,
		Execution:       Execution{ID: setup.InstanceID(), Generation: strconv.FormatUint(setup.Generation(), 10)},
		ScopeID:         string(grant.WireID()),
		ScopeGeneration: strconv.FormatUint(grant.Generation(), 10),
		Units:           []PlannedUnit{unit},
	}, nil
}

func (b *Bridge) plannedUnit(ctx context.Context, setup *frame.Setup) (PlannedUnit, error) {
	module, err := b.config.Source.Module(ctx, setup.Binding())
	if err != nil {
		return PlannedUnit{}, err
	}
	if module.Path == "" || module.Digest.Validate() != nil {
		return PlannedUnit{}, frame.ErrInvalid
	}
	configuration, err := b.publicConfiguration(ctx, setup)
	if err != nil {
		return PlannedUnit{}, err
	}
	binding := setup.Binding()
	unit := PlannedUnit{
		Instance:          Instance{ID: setup.InstanceID(), Generation: strconv.FormatUint(setup.Generation(), 10), Source: binding.Source, Configuration: binding.Configuration},
		ConfigurationJSON: configuration,
		Profile:           frame.BrowserJS,
		Module:            module,
		Imports:           []Import{},
		Exports:           setup.Unit().Exports,
	}
	for _, declaration := range binding.Imports {
		imported, err := b.plannedImport(ctx, setup, declaration)
		if err != nil {
			return PlannedUnit{}, err
		}
		unit.Imports = append(unit.Imports, imported)
	}
	return unit, nil
}

func (b *Bridge) publicConfiguration(ctx context.Context, setup *frame.Setup) (string, error) {
	if b.config.PublicConfiguration == nil {
		return emptyConfiguration, nil
	}
	configuration, err := b.config.PublicConfiguration(ctx, setup)
	if err != nil {
		return "", err
	}
	if len(configuration) == 0 {
		return emptyConfiguration, nil
	}
	if _, err := framevalue.ParseJSON(configuration); err != nil {
		return "", err
	}
	return string(configuration), nil
}

// plannedImport projects one approved import: an object of this Realm, a
// Portable transport bound by RemoteSource, or an accepted absence.
func (b *Bridge) plannedImport(ctx context.Context, setup *frame.Setup, declaration frame.ImportDeclaration) (Import, error) {
	values, err := setup.Imports(declaration.Slot)
	if err != nil {
		return Import{}, err
	}
	imported := Import{Slot: declaration.Slot, Contract: declaration.Contract, Kind: AbsentImportKind}
	switch len(values) {
	case 0:
		return imported, nil
	case 1:
	default:
		return Import{}, fmt.Errorf("%w: browser import slot requires one exact value", frame.ErrContractMismatch)
	}
	if object, ok := values[0].Value.(Object); ok {
		local := object.Reference.Export
		imported.Kind, imported.Local = LocalImportKind, &local
		return imported, nil
	}
	if declaration.Contract.Mode != frame.PortableMode || b.config.Remote == nil {
		return Import{}, frame.ErrContractMismatch
	}
	remote, err := b.config.Remote.Import(ctx, setup, values[0])
	if err != nil {
		return Import{}, err
	}
	if remote.Source != values[0].Source || remote.Token == "" {
		return Import{}, frame.ErrContractMismatch
	}
	imported.Kind, imported.Remote = RemoteImportKind, &remote
	return imported, nil
}

func (b *Bridge) releaseUnadmitted(ctx context.Context, entry *browserExecution) error {
	attempt := entry.attempt
	if entry.approvalUnconfirmed() {
		original, err := b.config.Store.Attempt(ctx, attempt.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !sameWire(original.Attempt, attempt) {
			return nil
		}
	}
	return b.config.Store.ReleaseAttempt(ctx, attempt.ID, unadmittedExitProof(attempt))
}

// closeExecution settles an execution the Runtime never admitted. An admitted
// execution stays unconfirmed until the Runtime proves its exit.
func (b *Bridge) closeExecution(ctx context.Context, entry *browserExecution) (frame.ReleaseState, error) {
	b.fence(entry)
	b.mu.Lock()
	admitted, owned := entry.runtimeOwnsExit(), entry.resourceHandle()
	registered := b.executions.get(entry.attempt.ID) == entry
	b.mu.Unlock()
	switch {
	case !registered && owned == nil:
		return frame.ResourceUnconfirmed, nil
	case !registered:
		return frame.ResourceReleased, nil
	case admitted:
		return frame.ResourceUnconfirmed, nil
	}
	if err := b.releaseUnadmitted(ctx, entry); err != nil {
		return frame.ResourceUnconfirmed, err
	}
	if err := owned.ConfirmReleased(nil); err != nil {
		return frame.ResourceUnconfirmed, err
	}
	b.mu.Lock()
	b.removeExecutionLocked(entry)
	b.reapRealmLocked(entry.plan.Realm)
	b.mu.Unlock()
	return frame.ResourceReleased, nil
}

func (b *Bridge) fence(entry *browserExecution) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry.fence()
}

func (b *Bridge) Poll(ctx context.Context, token string) (Poll, error) {
	session, err := b.anySession(token)
	if err != nil {
		return Poll{}, err
	}
	if err := cancellation.Err(ctx); err != nil {
		return Poll{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	result := Poll{Authority: b.authority, Realm: session.realm, Plans: []LocalPlan{}, Fences: []Fence{}}
	fences := make(map[Fence]bool)
	for _, entry := range b.executions.all() {
		if entry.plan.Realm == session.realm && entry.isFenced() && entry.runtimeOwnsExit() {
			fences[entry.scopeFence()] = true
		}
	}
	for _, entry := range b.executions.all() {
		if fences[entry.scopeFence()] || session.cleanupOnly {
			continue
		}
		if entry.plan.Realm == session.realm && !entry.isFenced() && !entry.hasReady() {
			result.Plans = append(result.Plans, copyWire(entry.plan))
		}
	}
	for fence := range fences {
		result.Fences = append(result.Fences, fence)
	}
	slices.SortFunc(result.Plans, func(a, b LocalPlan) int { return cmp.Compare(a.Execution.ID, b.Execution.ID) })
	slices.SortFunc(result.Fences, func(a, b Fence) int {
		return cmp.Or(cmp.Compare(a.ScopeID, b.ScopeID), cmp.Compare(a.ScopeGeneration, b.ScopeGeneration))
	})
	if err := b.checkResponse(result); err != nil {
		return Poll{}, err
	}
	return result, nil
}

func (b *Bridge) validateCurrentPlan(ctx context.Context, token string, plan LocalPlan) error {
	session, err := b.liveSession(token)
	if err != nil {
		return err
	}
	b.mu.Lock()
	entry := b.executions.get(plan.Execution.ID)
	valid := entry.serves(session.realm, plan.Execution) && sameWire(entry.plan, plan)
	b.mu.Unlock()
	if !valid {
		return frame.ErrStaleBinding
	}
	return entry.setup.ValidateCurrent(ctx)
}

// Ready observes a prepared execution. Only the Core Host publishes its
// exports, and a retry of the accepted report observes that same acceptance.
func (b *Bridge) Ready(ctx context.Context, token string, report ReadyReport) (PublicationResult, error) {
	if err := b.checkRequest(readyMethod, report); err != nil {
		return PublicationResult{}, err
	}
	session, err := b.liveSession(token)
	if err != nil {
		return PublicationResult{}, err
	}
	if report.Authority != b.authority || report.Realm != session.realm || report.OperationID == "" {
		return rejected(problemWrongAuthority), nil
	}
	digest, err := requestDigest(report)
	if err != nil {
		return PublicationResult{}, err
	}
	b.mu.Lock()
	entry := b.executions.get(report.Execution.ID)
	if session.cleanupOnly || !entry.runtimeOwnsExit() || !entry.serves(session.realm, report.Execution) {
		b.mu.Unlock()
		return rejected(problemExecutionUnavailable), nil
	}
	if result, replay, err := entry.replayReady(report.OperationID, digest); replay {
		b.mu.Unlock()
		return result, err
	}
	b.mu.Unlock()
	if err := b.validateCurrentPlan(ctx, token, entry.plan); err != nil {
		return PublicationResult{}, err
	}
	exports, problem := b.readyExports(entry.plan.Units[0], report)
	if problem != "" {
		return rejected(problem), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if session.cleanupOnly {
		return rejected(problemExecutionReleased), nil
	}
	result, accepted, err := entry.acceptReady(report.OperationID, digest)
	if accepted {
		entry.prepared <- exports
	}
	return result, err
}

// readyExports admits the reported objects against the approved unit: every
// declared export exactly once, plus dynamic members of declared Points, each
// with metadata that names its exact prepared object.
func (b *Bridge) readyExports(unit PlannedUnit, report ReadyReport) ([]frame.ExportValue, string) {
	objects, problem := readyObjects(unit, report.Objects)
	if problem != "" {
		return nil, problem
	}
	metadata, problem := readyMetadata(objects, report.Contributions)
	if problem != "" {
		return nil, problem
	}
	exports := make([]frame.ExportValue, 0, len(report.Objects))
	for _, declaration := range unit.Exports {
		reference, exists := objects.byExport[declaration.Name]
		if !exists || reference.Export.Contract != declaration.Contract {
			return nil, problemUndeclaredExports
		}
		var value any
		if declaration.Contract.Kind == frame.PointKind {
			encoded, exists := metadata[reference.ObjectID]
			if !exists {
				encoded = jsonNull
			}
			decoded, err := b.config.Host.DecodeMetadata(declaration.Contract, []byte(encoded))
			if err != nil {
				return nil, problemInvalidPointMetadata
			}
			value = decoded
		}
		exports = append(exports, frame.ExportValue{Name: declaration.Name, Contract: declaration.Contract, Metadata: value, Value: Object{Reference: reference}})
		delete(objects.byExport, declaration.Name)
	}
	// Dynamic setup contributions require a declared Point and are returned in
	// the same prepared batch. They acquire visibility only in the Core owner.
	for _, reference := range objects.byExport {
		if reference.Export.Contract.Kind != frame.PointKind || !declaresContract(unit, reference.Export.Contract) {
			return nil, problemUndeclaredMember
		}
		encoded, exists := metadata[reference.ObjectID]
		if !exists {
			return nil, problemMissingMetadata
		}
		decoded, err := b.config.Host.DecodeMetadata(reference.Export.Contract, []byte(encoded))
		if err != nil {
			return nil, problemInvalidMemberData
		}
		exports = append(exports, frame.ExportValue{Name: reference.Export.Name, Contract: reference.Export.Contract, Metadata: decoded, Value: Object{Reference: reference}})
	}
	return exports, ""
}

func declaresContract(unit PlannedUnit, contract frame.ContractRef) bool {
	return slices.ContainsFunc(unit.Exports, func(declaration frame.ExportDeclaration) bool { return declaration.Contract == contract })
}

type readyObjectIndex struct {
	byExport map[string]ObjectRef
	byID     map[string]ObjectRef
}

func readyObjects(unit PlannedUnit, references []ObjectRef) (readyObjectIndex, string) {
	index := readyObjectIndex{byExport: make(map[string]ObjectRef, len(references)), byID: make(map[string]ObjectRef, len(references))}
	for _, reference := range references {
		if !reference.validIn(unit.Instance.ID) {
			return readyObjectIndex{}, problemInvalidObjectSource
		}
		if _, exists := index.byExport[reference.Export.Name]; exists {
			return readyObjectIndex{}, problemDuplicateExport
		}
		if _, exists := index.byID[reference.ObjectID]; exists {
			return readyObjectIndex{}, problemDuplicateObject
		}
		index.byExport[reference.Export.Name] = reference
		index.byID[reference.ObjectID] = reference
	}
	return index, ""
}

func readyMetadata(objects readyObjectIndex, contributions []PreparedContribution) (map[string]string, string) {
	metadata := make(map[string]string, len(contributions))
	for _, contribution := range contributions {
		reference, exists := objects.byID[contribution.Object.ObjectID]
		if !exists || reference != contribution.Object || reference.Export.Contract.Kind != frame.PointKind {
			return nil, problemInexactPoint
		}
		if _, exists := metadata[reference.ObjectID]; exists {
			return nil, problemDuplicateMetadata
		}
		metadata[reference.ObjectID] = contribution.MetadataJSON
	}
	return metadata, ""
}
