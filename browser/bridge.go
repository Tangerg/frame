package browser

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
	framevalue "github.com/Tangerg/frame/value"
)

type ModuleSource interface {
	Module(context.Context, frame.UnitBinding) (Artifact, error)
}

// RemoteSource binds a declared Portable transport to an exact approved import.
// The adapter registers all sessions and call resources with the original Setup
// before returning. A descriptor alone never creates a callable transport.
type RemoteSource interface {
	Import(context.Context, *frame.Setup, frame.BoundValue) (RemoteImport, error)
}

type BridgeConfig struct {
	Host   *frame.Host
	Store  *sqlite.Store
	Source ModuleSource
	Remote RemoteSource
	// PublicConfiguration is the consumer's explicit public projection. Opaque
	// installation configuration may contain credentials and is never sent by
	// default. The instance keeps the original configuration digest as identity.
	PublicConfiguration func(context.Context, *frame.Setup) ([]byte, error)
	MaxRealms           int
	MaxPending          int
	MaxMessageBytes     int64
	Points              []frame.ContractRef
}

// Credentials authenticate one original Realm. They are returned only to the
// trusted application's bootstrap channel, never to plugin modules or URLs.
type Credentials struct {
	MaxMessageBytes int64     `json:"maxMessageBytes"`
	Authority       Authority `json:"authority"`
	Realm           Realm     `json:"realm"`
	Token           string    `json:"token"`
	ResumeToken     string    `json:"resumeToken"`
}

// Object is a reference to an actual object owned by the browser Runtime. It is
// not a cross-runtime implementation of the object's native methods.
type Object struct {
	Reference ObjectRef
}

type Bridge struct {
	config         BridgeConfig
	authority      Authority
	mu             sync.Mutex
	sessions       map[string]*realmSession
	signer         realmSigner
	receiptReaders map[string]*receiptRead
	preparing      int
	openingRealms  int
	openingRoutes  int
	executions     map[string]*browserExecution
	operations     map[string]*controlOperation
	publications   map[string]*controlOperation
	snapshots      map[string]*snapshotLease
	uses           map[string]*browserUse
	routes         map[string]browserRoute
	openings       map[string]*realmOpening
}

type realmOpening struct {
	verifier    framevalue.Digest
	done        chan struct{}
	credentials Credentials
	failure     error
}

type realmSession struct {
	realm       Realm
	application *frame.Lifetime

	cleanupOnly     bool
	revision        uint64
	projectionBytes int64
}

type browserExecution struct {
	plan              LocalPlan
	attempt           sqlite.Attempt
	setup             *frame.Setup
	resource          *frame.ResourceHandle
	ready             chan preparedResult
	readyOperation    string
	readyDigest       framevalue.Digest
	retiring          bool
	uncertainApproval bool
	admitted          bool
	projectionBytes   int64
}

type preparedResult struct {
	exports []frame.ExportValue
	err     error
}
type controlOperation struct {
	kind               string
	realm              Realm
	digest             framevalue.Digest
	execution          string
	parentID           string
	publication        PublicationResult
	grant              *UseGrant
	done               chan struct{}
	failure            error
	receiptReservation *sqlite.ReceiptReservation
}
type browserUse struct {
	grant     UseGrant
	use       *frame.Use
	resource  *frame.ResourceHandle
	releaseID string
}
type browserRoute struct {
	grant    RouteGrant
	original *frame.EventRoute
}

func NewBridge(config BridgeConfig) (*Bridge, error) {
	if config.Host == nil || config.Store == nil || config.Source == nil || !config.Host.UsesAuthority(config.Store) {
		return nil, frame.ErrInvalid
	}
	if config.MaxRealms == 0 {
		config.MaxRealms = defaultMaxRealms
	}
	if config.MaxPending == 0 {
		config.MaxPending = defaultMaxPending
	}
	if config.MaxMessageBytes == 0 {
		config.MaxMessageBytes = defaultMaxMessageBytes
	}
	if config.MaxRealms < 1 || config.MaxPending < 1 || config.MaxMessageBytes < 1 {
		return nil, frame.ErrInvalid
	}
	config.Points = slices.Clone(config.Points)
	return &Bridge{
		config:         config,
		authority:      Authority{ID: config.Store.ID(), Epoch: strconv.FormatUint(config.Store.Epoch(), 10)},
		signer:         newRealmSigner(),
		sessions:       make(map[string]*realmSession),
		openings:       make(map[string]*realmOpening),
		executions:     make(map[string]*browserExecution),
		operations:     make(map[string]*controlOperation),
		receiptReaders: make(map[string]*receiptRead),
		publications:   make(map[string]*controlOperation),
		snapshots:      make(map[string]*snapshotLease),
		uses:           make(map[string]*browserUse),
		routes:         make(map[string]browserRoute),
	}, nil
}

func (*Bridge) Profile() frame.Profile  { return frame.BrowserJS }
func (*Bridge) RealmBound() bool        { return true }
func (b *Bridge) Authority() Authority  { return b.authority }
func RealmID(realm Realm) frame.RealmID { return frame.RealmID(realm.ID + ":" + realm.Epoch) }

// Credentials is a trusted in-process composition port. It projects the
// original authenticated session and is deliberately absent from Handler.
// The caller already owns application admission; no second Realm map is needed.
func (b *Bridge) Credentials(realm Realm) (Credentials, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session := b.sessions[string(RealmID(realm))]
	if session == nil || session.realm != realm {
		return Credentials{}, frame.ErrUnavailable
	}
	return Credentials{MaxMessageBytes: b.config.MaxMessageBytes, Authority: b.authority, Realm: realm, Token: b.signer.token(realm)}, nil
}

// OpenRealm requires caller-generated credentials before dispatch. Repeating
// the exact operation after a lost response recovers its original Realm, even
// after Host reconstruction; no raw resume credential is persisted.
func (b *Bridge) OpenRealm(ctx context.Context, operationID, resumeToken string) (Credentials, error) {
	if frame.InstallationID(operationID).Validate() != nil || len(resumeToken) < 32 || len(resumeToken) > 512 {
		return Credentials{}, frame.ErrInvalid
	}
	verifier := framevalue.Hash([]byte(resumeToken))
	b.mu.Lock()
	if previous := b.openings[operationID]; previous != nil {
		if subtle.ConstantTimeCompare([]byte(previous.verifier.String()), []byte(verifier.String())) != 1 {
			b.mu.Unlock()
			return Credentials{}, frame.ErrFenced
		}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return Credentials{}, ctx.Err()
		case <-previous.done:
			return previous.credentials, previous.failure
		}
	}
	if len(b.openings) >= b.config.MaxRealms {
		b.mu.Unlock()
		return Credentials{}, frame.ErrCapacity
	}
	opening := &realmOpening{verifier: verifier, done: make(chan struct{})}
	b.openings[operationID] = opening
	b.mu.Unlock()
	credentials, err := b.openRealm(ctx, operationID, resumeToken)
	b.mu.Lock()
	opening.credentials = credentials
	opening.failure = err
	close(opening.done)
	delete(b.openings, operationID)
	b.mu.Unlock()
	return credentials, err
}

func (b *Bridge) openRealm(ctx context.Context, operationID, resumeToken string) (Credentials, error) {
	previous, err := b.config.Store.RealmOperation(ctx, operationID)
	if err == nil {
		return b.ResumeRealm(ctx, Realm{ID: previous.ID, Epoch: previous.Epoch}, resumeToken)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Credentials{}, err
	}
	credentials := Credentials{MaxMessageBytes: b.config.MaxMessageBytes, Authority: b.authority, Realm: Realm{ID: newToken(), Epoch: newToken()}, ResumeToken: resumeToken}
	b.mu.Lock()
	if len(b.sessions)+b.openingRealms >= b.config.MaxRealms {
		b.mu.Unlock()
		return Credentials{}, frame.ErrCapacity
	}
	b.openingRealms++
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.openingRealms--; b.mu.Unlock() }()
	credentials.Token = b.signer.token(credentials.Realm)
	if err := b.config.Store.RegisterRealm(ctx, sqlite.RealmCredential{ID: credentials.Realm.ID, Epoch: credentials.Realm.Epoch, OperationID: operationID, ResumeVerifier: framevalue.Hash([]byte(credentials.ResumeToken))}); err != nil {
		return Credentials{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	session := &realmSession{realm: credentials.Realm, application: frame.NewLifetime(context.Background())}
	b.sessions[string(RealmID(credentials.Realm))] = session

	return credentials, nil
}

func (b *Bridge) ResumeRealm(ctx context.Context, realm Realm, resumeToken string) (Credentials, error) {
	credential, err := b.config.Store.RealmCredential(ctx, realm.ID, realm.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Credentials{}, errors.Join(frame.ErrFenced, err)
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("browser realm credential: %w", err)
	}
	actual := framevalue.Hash([]byte(resumeToken))
	if subtle.ConstantTimeCompare([]byte(actual.String()), []byte(credential.ResumeVerifier.String())) != 1 {
		return Credentials{}, frame.ErrFenced
	}
	return Credentials{MaxMessageBytes: b.config.MaxMessageBytes, Authority: b.authority, Realm: realm, Token: b.signer.token(realm), ResumeToken: resumeToken}, nil
}

func (b *Bridge) session(token string, normal bool) (*realmSession, error) {
	realm, ok := b.signer.authenticate(token)
	if !ok {
		return nil, frame.ErrFenced
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sessions[string(RealmID(realm))]
	if s == nil {
		if normal {
			return nil, frame.ErrFenced
		}
		return &realmSession{realm: realm, cleanupOnly: true}, nil
	}
	if normal && s.cleanupOnly {
		return nil, frame.ErrFenced
	}
	return s, nil
}

func (b *Bridge) Prepare(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
	if !b.config.Host.OwnsSetup(setup) {
		return nil, frame.ErrFenced
	}
	b.mu.Lock()
	session := b.sessions[string(setup.Realm())]
	if session == nil || session.cleanupOnly {
		b.mu.Unlock()
		return nil, frame.ErrFenced
	}
	if len(b.executions)+b.preparing >= b.config.MaxPending {
		b.mu.Unlock()
		return nil, frame.ErrCapacity
	}
	realm := session.realm
	b.preparing++
	b.mu.Unlock()
	reserved := true
	defer func() {
		if reserved {
			b.mu.Lock()
			b.preparing--
			b.mu.Unlock()
		}
	}()
	module, err := b.config.Source.Module(ctx, setup.Binding())
	if err != nil {
		return nil, err
	}
	if module.Path == "" || module.Digest.Validate() != nil {
		return nil, frame.ErrInvalid
	}
	unit := setup.Unit()
	configuration := []byte(`{}`)
	if b.config.PublicConfiguration != nil {
		configuration, err = b.config.PublicConfiguration(ctx, setup)
		if err != nil {
			return nil, err
		}
	}
	if len(configuration) == 0 {
		configuration = []byte(`{}`)
	}
	if _, err := framevalue.ParseJSON(configuration); err != nil {
		return nil, err
	}
	planned := PlannedUnit{Instance: Instance{ID: setup.InstanceID(), Generation: strconv.FormatUint(setup.Generation(), 10), Source: setup.Binding().Source, Configuration: setup.Binding().Configuration}, ConfigurationJSON: string(configuration), Profile: frame.BrowserJS, Module: module, Imports: []Import{}, Exports: unit.Exports}
	for _, declaration := range setup.Binding().Imports {
		values, err := setup.Imports(declaration.Slot)
		if err != nil {
			return nil, err
		}
		if len(values) > 1 {
			return nil, fmt.Errorf("%w: browser import slot requires one exact value", frame.ErrContractMismatch)
		}
		imported := Import{Slot: declaration.Slot, Contract: declaration.Contract, Kind: AbsentImportKind}
		if len(values) == 1 {
			object, ok := values[0].Value.(Object)
			if ok {
				imported.Kind = LocalImportKind
				reference := object.Reference.Export
				imported.Local = &reference
			} else {
				if declaration.Contract.Mode != frame.PortableMode || b.config.Remote == nil {
					return nil, frame.ErrContractMismatch
				}
				remote, err := b.config.Remote.Import(ctx, setup, values[0])
				if err != nil {
					return nil, err
				}
				if remote.Source != values[0].Source || remote.Token == "" {
					return nil, frame.ErrContractMismatch
				}
				imported.Kind = RemoteImportKind
				imported.Remote = &remote
			}
		}
		planned.Imports = append(planned.Imports, imported)
	}
	plan := LocalPlan{Protocol: Protocol, ApprovalToken: newToken(), Authority: b.authority, Realm: realm, Execution: Execution{ID: setup.InstanceID(), Generation: strconv.FormatUint(setup.Generation(), 10)}, ScopeID: string(setup.ScopeGrant().WireID()), ScopeGeneration: strconv.FormatUint(setup.ScopeGeneration(), 10), Units: []PlannedUnit{planned}}
	attempt := sqlite.Attempt{ID: setup.InstanceID(), Epoch: setup.Epoch(), Source: setup.Binding().Source, Binding: setup.Snapshot(), Realm: string(RealmID(realm)), Profile: frame.BrowserJS, Scope: setup.ScopeGrant().WireID(), ScopeGeneration: setup.ScopeGeneration(), Generation: setup.Generation()}
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
	entry := &browserExecution{plan: plan, attempt: attempt, setup: setup, ready: make(chan preparedResult, 1), projectionBytes: projectionBytes}
	registered := make(chan struct{})
	resource, err := setup.Own(frame.Resource{Name: "browser execution " + attempt.ID, Cancel: func(error) { b.fence(entry) }, Close: func(wait context.Context) (frame.ReleaseState, error) {
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
	entry.resource = resource
	entry.uncertainApproval = approvalErr != nil
	b.preparing--
	reserved = false
	b.executions[plan.Execution.ID] = entry
	projectionHeld = false
	revoked := approvalErr != nil || session.cleanupOnly || entry.retiring || ctx.Err() != nil
	if revoked {
		entry.retiring = true
	}
	session.revision++
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
	case result := <-entry.ready:
		return result.exports, result.err
	case <-ctx.Done():
		b.fence(entry)
		return nil, context.Cause(ctx)
	}
}

func (b *Bridge) releaseUnadmitted(ctx context.Context, entry *browserExecution) error {
	a := entry.attempt
	if entry.uncertainApproval {
		original, err := b.config.Store.Attempt(ctx, a.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !equal(original.Attempt, a) {
			return nil
		}
	}
	return b.config.Store.ReleaseAttempt(ctx, a.ID, unadmittedExitProof(a))
}

func (b *Bridge) closeExecution(ctx context.Context, entry *browserExecution) (frame.ReleaseState, error) {
	b.fence(entry)
	b.mu.Lock()
	admitted := entry.admitted
	owned := entry.resource
	registered := b.executions[entry.attempt.ID] == entry
	b.mu.Unlock()
	if !registered {
		if owned == nil {
			return frame.ResourceUnconfirmed, nil
		}
		return frame.ResourceReleased, nil
	}
	if admitted {
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

func (b *Bridge) reapRealmLocked(realm Realm) {
	id := string(RealmID(realm))
	s := b.sessions[id]
	if s == nil || !s.cleanupOnly || s.projectionBytes != 0 {
		return
	}
	for _, e := range b.executions {
		if e.plan.Realm == realm {
			return
		}
	}
	delete(b.sessions, id)
}

func (b *Bridge) fence(entry *browserExecution) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !entry.retiring {
		entry.retiring = true
		if s := b.sessions[entry.attempt.Realm]; s != nil {
			s.revision++
		}
	}
}

func (b *Bridge) Poll(ctx context.Context, token string) (Poll, error) {
	session, err := b.session(token, false)
	if err != nil {
		return Poll{}, err
	}
	if err := ctx.Err(); err != nil {
		return Poll{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	result := Poll{Authority: b.authority, Realm: session.realm, Revision: strconv.FormatUint(session.revision, 10), Plans: []LocalPlan{}, Fences: []Fence{}}
	fences := map[Fence]struct{}{}
	for _, entry := range b.executions {
		if entry.plan.Realm == session.realm && entry.retiring && entry.admitted {
			fences[Fence{ScopeID: entry.plan.ScopeID, ScopeGeneration: entry.plan.ScopeGeneration}] = struct{}{}
		}
	}
	for _, entry := range b.executions {
		if _, fenced := fences[Fence{ScopeID: entry.plan.ScopeID, ScopeGeneration: entry.plan.ScopeGeneration}]; fenced {
			continue
		}
		if entry.plan.Realm == session.realm && !entry.retiring && entry.readyOperation == "" && !session.cleanupOnly {
			result.Plans = append(result.Plans, clone(entry.plan))
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
	session, err := b.session(token, true)
	if err != nil {
		return err
	}
	b.mu.Lock()
	entry := b.executions[plan.Execution.ID]
	valid := entry != nil && !entry.retiring && entry.plan.Realm == session.realm && equal(entry.plan, plan)
	b.mu.Unlock()
	if !valid {
		return frame.ErrStaleBinding
	}
	return entry.setup.ValidateCurrent(ctx)
}

func (b *Bridge) Ready(ctx context.Context, token string, report ReadyReport) (PublicationResult, error) {
	if err := b.checkRequest(readyMethod, report); err != nil {
		return PublicationResult{}, err
	}
	session, err := b.session(token, true)
	if err != nil {
		return PublicationResult{}, err
	}
	if report.Authority != b.authority || report.Realm != session.realm || report.OperationID == "" {
		return PublicationResult{Decision: Rejected, Problem: "wrong execution authority"}, nil
	}
	digest, err := requestDigest(report)
	if err != nil {
		return PublicationResult{}, err
	}
	b.mu.Lock()
	entry := b.executions[report.Execution.ID]
	if session.cleanupOnly || entry == nil || !entry.admitted || entry.plan.Execution != report.Execution || entry.plan.Realm != session.realm || entry.retiring {
		b.mu.Unlock()
		return PublicationResult{Decision: Rejected, Problem: "execution unavailable"}, nil
	}
	if entry.readyOperation != "" {
		same := entry.readyOperation == report.OperationID && entry.readyDigest == digest
		b.mu.Unlock()
		if !same {
			return PublicationResult{}, sqlite.ErrConflict
		}
		return PublicationResult{Decision: Accepted}, nil
	}
	b.mu.Unlock()
	if err := b.validateCurrentPlan(ctx, token, entry.plan); err != nil {
		return PublicationResult{}, err
	}
	unit := entry.plan.Units[0]
	objects := map[string]ObjectRef{}
	metadata := map[string]string{}
	for _, contribution := range report.Contributions {
		if _, exists := metadata[contribution.Object.ObjectID]; exists {
			return PublicationResult{Decision: Rejected, Problem: "duplicate prepared object"}, nil
		}
		metadata[contribution.Object.ObjectID] = contribution.MetadataJSON
	}
	for _, reference := range report.Objects {
		if reference.ObjectID == "" || reference.Export.InstanceID != unit.Instance.ID {
			return PublicationResult{Decision: Rejected, Problem: "invalid object source"}, nil
		}
		if _, exists := objects[reference.Export.Name]; exists {
			return PublicationResult{Decision: Rejected, Problem: "duplicate export name"}, nil
		}
		objects[reference.Export.Name] = reference
	}
	exports := make([]frame.ExportValue, 0, len(report.Objects))
	for _, declaration := range unit.Exports {
		reference, exists := objects[declaration.Name]
		if !exists || reference.Export.Contract != declaration.Contract {
			return PublicationResult{Decision: Rejected, Problem: "actual exports differ from approved declarations"}, nil
		}
		var value any
		if declaration.Contract.Kind == frame.PointKind {
			encoded, exists := metadata[reference.ObjectID]
			if !exists {
				encoded = "null"
			}
			value, err = b.config.Host.DecodeMetadata(declaration.Contract, []byte(encoded))
			if err != nil {
				return PublicationResult{Decision: Rejected, Problem: "invalid point metadata"}, nil
			}
		}
		exports = append(exports, frame.ExportValue{Name: declaration.Name, Contract: declaration.Contract, Metadata: value, Value: Object{Reference: reference}, Durable: true})
		delete(objects, declaration.Name)
	}
	// Dynamic setup contributions require a declared Point and are returned in
	// the same prepared batch. They acquire visibility only in the Core owner.
	for _, reference := range objects {
		if reference.Export.Contract.Kind != frame.PointKind || !slices.ContainsFunc(unit.Exports, func(declaration frame.ExportDeclaration) bool {
			return declaration.Contract == reference.Export.Contract
		}) {
			return PublicationResult{Decision: Rejected, Problem: "undeclared prepared contribution"}, nil
		}
		encoded, exists := metadata[reference.ObjectID]
		if !exists {
			return PublicationResult{Decision: Rejected, Problem: "missing prepared metadata"}, nil
		}
		value, err := b.config.Host.DecodeMetadata(reference.Export.Contract, []byte(encoded))
		if err != nil {
			return PublicationResult{Decision: Rejected, Problem: "invalid prepared metadata"}, nil
		}
		exports = append(exports, frame.ExportValue{Name: reference.Export.Name, Contract: reference.Export.Contract, Metadata: value, Value: Object{Reference: reference}})
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if session.cleanupOnly || entry.retiring {
		return PublicationResult{Decision: Rejected, Problem: "execution already released"}, nil
	}
	if entry.readyOperation != "" {
		if entry.readyOperation != report.OperationID || entry.readyDigest != digest {
			return PublicationResult{}, sqlite.ErrConflict
		}
		return PublicationResult{Decision: Accepted}, nil
	}
	entry.readyOperation = report.OperationID
	entry.readyDigest = digest

	session.revision++
	entry.ready <- preparedResult{exports: exports}
	return PublicationResult{Decision: Accepted}, nil
}

func newToken() string {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(token[:])
}
func equal(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}
func clone[T any](value T) T {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var copy T
	if err := json.Unmarshal(encoded, &copy); err != nil {
		panic(err)
	}
	return copy
}
func requestDigest(value any) (framevalue.Digest, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return framevalue.Digest{}, err
	}
	canonical, err := framevalue.ParseJSON(encoded)
	if err != nil {
		return framevalue.Digest{}, err
	}
	return canonical.Digest(), nil
}
