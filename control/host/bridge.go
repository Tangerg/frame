package host

import (
	"bytes"
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
	"github.com/Tangerg/frame/control"
	"github.com/Tangerg/frame/integrity"
	"github.com/Tangerg/frame/jsonvalue"
	"github.com/Tangerg/frame/sqlite"
)

type ModuleSource interface {
	Module(context.Context, frame.UnitBinding) (control.Artifact, error)
}

// RemoteSource binds a declared Portable transport to an exact approved import.
// The adapter registers all sessions and call resources with the original Setup
// before returning. A descriptor alone never creates a callable transport.
type RemoteSource interface {
	Import(context.Context, *frame.Setup, frame.BoundValue) (control.RemoteImport, error)
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
	MaxRequestBytes     int64
	Points              []frame.ContractRef
}

// Credentials authenticate one original Realm. They are returned only to the
// trusted application's bootstrap channel, never to plugin modules or URLs.
type Credentials struct {
	Authority   control.Authority `json:"authority"`
	Realm       control.Realm     `json:"realm"`
	Token       string            `json:"token"`
	ResumeToken string            `json:"resumeToken"`
}

// Object is a reference to an actual object owned by the browser Runtime. It is
// not a cross-runtime implementation of the object's native methods.
type Object struct {
	Reference control.ObjectRef
}

type Bridge struct {
	config     BridgeConfig
	authority  control.Authority
	mu         sync.Mutex
	sessions   map[string]*realmSession
	tokens     map[string]*realmSession
	executions map[string]*browserExecution
	operations map[string]*controlOperation
	snapshots  map[string]*wireSnapshot
	uses       map[string]*browserUse
	routes     map[string]browserRoute
	openings   map[string]*realmOpening
}

type realmOpening struct {
	verifier    integrity.Digest
	done        chan struct{}
	credentials Credentials
	failure     error
}

type realmSession struct {
	realm       control.Realm
	token       string
	cleanupOnly bool
	revision    uint64
	fences      map[control.Fence]struct{}
}

type browserExecution struct {
	plan           control.LocalPlan
	attempt        sqlite.Attempt
	setup          *frame.Setup
	resource       *frame.ResourceHandle
	ready          chan preparedResult
	readyOperation string
	readyDigest    integrity.Digest
	objects        []control.ObjectRef
	released       bool
	exposed        bool
}

type preparedResult struct {
	exports []frame.ExportValue
	err     error
}
type controlOperation struct {
	kind        string
	realm       control.Realm
	digest      integrity.Digest
	execution   string
	parentID    string
	publication control.PublicationResult
	grant       *control.UseGrant
	done        chan struct{}
	failure     error
	withdrawn   bool
	released    bool
}
type wireSnapshot struct {
	realm      string
	original   *frame.Snapshot
	points     map[string]frame.ContractRef
	candidates map[string]frame.CandidateRef
}
type browserUse struct {
	grant     control.UseGrant
	use       *frame.Use
	resource  *frame.ResourceHandle
	releaseID string
}
type browserRoute struct {
	grant    control.RouteGrant
	original *frame.EventRoute
}

func NewBridge(config BridgeConfig) (*Bridge, error) {
	if config.Host == nil || config.Store == nil || config.Source == nil || !config.Host.UsesAuthority(config.Store) {
		return nil, frame.ErrInvalid
	}
	if config.MaxRealms == 0 {
		config.MaxRealms = 64
	}
	if config.MaxPending == 0 {
		config.MaxPending = 4096
	}
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = 4 << 20
	}
	if config.MaxRealms < 1 || config.MaxPending < 1 || config.MaxRequestBytes < 1 {
		return nil, frame.ErrInvalid
	}
	return &Bridge{config: config, authority: control.Authority{ID: config.Store.ID(), Epoch: strconv.FormatUint(config.Store.Epoch(), 10)}, sessions: map[string]*realmSession{}, tokens: map[string]*realmSession{}, executions: map[string]*browserExecution{}, operations: map[string]*controlOperation{}, snapshots: map[string]*wireSnapshot{}, uses: map[string]*browserUse{}, routes: map[string]browserRoute{}, openings: map[string]*realmOpening{}}, nil
}

func (*Bridge) Profile() frame.Profile          { return frame.BrowserJS }
func (*Bridge) RealmBound() bool                { return true }
func (b *Bridge) Authority() control.Authority  { return b.authority }
func RealmID(realm control.Realm) frame.RealmID { return frame.RealmID(realm.ID + ":" + realm.Epoch) }

// Credentials is a trusted in-process composition port. It projects the
// original authenticated session and is deliberately absent from Handler.
// The caller already owns application admission; no second Realm map is needed.
func (b *Bridge) Credentials(realm control.Realm) (Credentials, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session := b.sessions[string(RealmID(realm))]
	if session == nil || session.realm != realm {
		return Credentials{}, frame.ErrUnavailable
	}
	return Credentials{Authority: b.authority, Realm: realm, Token: session.token}, nil
}

// OpenRealm requires caller-generated credentials before dispatch. Repeating
// the exact operation after a lost response recovers its original Realm, even
// after Host reconstruction; no raw resume credential is persisted.
func (b *Bridge) OpenRealm(ctx context.Context, operationID, resumeToken string) (Credentials, error) {
	if frame.InstallationID(operationID).Validate() != nil || len(resumeToken) < 32 || len(resumeToken) > 512 {
		return Credentials{}, frame.ErrInvalid
	}
	verifier := integrity.Hash([]byte(resumeToken))
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
	opening.credentials = credentials
	opening.failure = err
	close(opening.done)
	if err != nil {
		b.mu.Lock()
		if b.openings[operationID] == opening {
			delete(b.openings, operationID)
		}
		b.mu.Unlock()
	}
	return credentials, err
}

func (b *Bridge) openRealm(ctx context.Context, operationID, resumeToken string) (Credentials, error) {
	previous, err := b.config.Store.RealmOperation(ctx, operationID)
	if err == nil {
		return b.ResumeRealm(ctx, control.Realm{ID: previous.ID, Epoch: previous.Epoch}, resumeToken)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Credentials{}, err
	}
	credentials := Credentials{Authority: b.authority, Realm: control.Realm{ID: newToken(), Epoch: newToken()}, Token: newToken(), ResumeToken: resumeToken}
	b.mu.Lock()
	if len(b.sessions) >= b.config.MaxRealms {
		b.mu.Unlock()
		return Credentials{}, frame.ErrCapacity
	}
	b.mu.Unlock()
	if err := b.config.Store.RegisterRealm(ctx, sqlite.RealmCredential{ID: credentials.Realm.ID, Epoch: credentials.Realm.Epoch, OperationID: operationID, ResumeVerifier: integrity.Hash([]byte(credentials.ResumeToken))}); err != nil {
		return Credentials{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sessions) >= b.config.MaxRealms {
		return Credentials{}, frame.ErrCapacity
	}
	session := &realmSession{realm: credentials.Realm, token: credentials.Token, fences: map[control.Fence]struct{}{}}
	b.sessions[string(RealmID(credentials.Realm))] = session
	b.tokens[credentials.Token] = session
	return credentials, nil
}

func (b *Bridge) ResumeRealm(ctx context.Context, realm control.Realm, resumeToken string) (Credentials, error) {
	credential, err := b.config.Store.RealmCredential(ctx, realm.ID, realm.Epoch)
	if err != nil {
		return Credentials{}, frame.ErrFenced
	}
	actual := integrity.Hash([]byte(resumeToken))
	if subtle.ConstantTimeCompare([]byte(actual.String()), []byte(credential.ResumeVerifier.String())) != 1 {
		return Credentials{}, frame.ErrFenced
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := string(RealmID(realm))
	if old := b.sessions[key]; old != nil {
		return Credentials{Authority: b.authority, Realm: realm, Token: old.token, ResumeToken: resumeToken}, nil
	}
	if len(b.sessions) >= b.config.MaxRealms {
		return Credentials{}, frame.ErrCapacity
	}
	session := &realmSession{realm: realm, token: newToken(), cleanupOnly: true, fences: map[control.Fence]struct{}{}}
	b.sessions[key] = session
	b.tokens[session.token] = session
	return Credentials{Authority: b.authority, Realm: realm, Token: session.token, ResumeToken: resumeToken}, nil
}

func (b *Bridge) session(token string, normal bool) (*realmSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session := b.tokens[token]
	if session == nil || normal && session.cleanupOnly {
		return nil, frame.ErrFenced
	}
	return session, nil
}

func (b *Bridge) Prepare(ctx context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
	if setup == nil || setup.Epoch() != b.config.Store.Epoch() {
		return nil, frame.ErrFenced
	}
	b.mu.Lock()
	session := b.sessions[string(setup.Realm())]
	if session == nil || session.cleanupOnly {
		b.mu.Unlock()
		return nil, frame.ErrFenced
	}
	if len(b.executions) >= b.config.MaxPending {
		b.mu.Unlock()
		return nil, frame.ErrCapacity
	}
	realm := session.realm
	b.mu.Unlock()
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
	if _, err := jsonvalue.Parse(configuration); err != nil {
		return nil, err
	}
	planned := control.PlannedUnit{Instance: control.Instance{ID: setup.InstanceID(), Generation: strconv.FormatUint(setup.Generation(), 10), Source: setup.Binding().Source, Configuration: setup.Binding().Configuration}, ConfigurationJSON: string(configuration), Profile: frame.BrowserJS, Module: module, Imports: []control.Import{}, Exports: unit.Exports}
	for _, declaration := range setup.Binding().Imports {
		values, err := setup.Imports(declaration.Slot)
		if err != nil {
			return nil, err
		}
		if len(values) > 1 {
			return nil, fmt.Errorf("%w: browser import slot requires one exact value", frame.ErrContractMismatch)
		}
		imported := control.Import{Slot: declaration.Slot, Contract: declaration.Contract, Kind: control.AbsentImportKind}
		if len(values) == 1 {
			object, ok := values[0].Value.(Object)
			if ok {
				imported.Kind = control.LocalImportKind
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
				imported.Kind = control.RemoteImportKind
				imported.Remote = &remote
			}
		}
		planned.Imports = append(planned.Imports, imported)
	}
	plan := control.LocalPlan{Protocol: control.Protocol, ApprovalToken: newToken(), Authority: b.authority, Realm: realm, Execution: control.Execution{ID: setup.InstanceID(), Generation: strconv.FormatUint(setup.Generation(), 10)}, ScopeID: string(setup.ScopeGrant().WireID()), ScopeGeneration: strconv.FormatUint(setup.ScopeGeneration(), 10), Units: []control.PlannedUnit{planned}}
	attempt := sqlite.Attempt{ID: setup.InstanceID(), Epoch: setup.Epoch(), Source: setup.Binding().Source, Binding: setup.Snapshot(), Realm: string(RealmID(realm)), Profile: frame.BrowserJS, Scope: setup.ScopeGrant().WireID(), ScopeGeneration: setup.ScopeGeneration(), Generation: setup.Generation()}
	if err := b.config.Store.RecordAttempt(ctx, attempt); err != nil {
		return nil, err
	}
	entry := &browserExecution{plan: plan, attempt: attempt, setup: setup, ready: make(chan preparedResult, 1)}
	resource, err := setup.Own(frame.Resource{Name: "browser execution " + attempt.ID, Cancel: func(error) { b.fence(entry) }, Close: func(context.Context) (frame.ReleaseState, error) {
		b.fence(entry)
		b.mu.Lock()
		released := entry.released
		b.mu.Unlock()
		if released {
			return frame.ResourceReleased, nil
		}
		return frame.ResourceUnconfirmed, nil
	}})
	if err != nil {
		proof := sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: attempt.ID + ":not-delivered", Kind: "not-delivered", Evidence: []byte(`{"delivered":false}`)}
		return nil, errors.Join(err, b.config.Store.ReleaseAttempt(context.Background(), attempt.ID, proof))
	}
	entry.resource = resource
	b.mu.Lock()
	if session.cleanupOnly {
		entry.released = true
		b.mu.Unlock()
		proof := sqlite.ExitProof{AttemptID: attempt.ID, Realm: attempt.Realm, ExitID: attempt.ID + ":not-delivered", Kind: "not-delivered", Evidence: []byte(`{"delivered":false}`)}
		persisted := b.config.Store.ReleaseAttempt(context.Background(), attempt.ID, proof)
		if persisted == nil {
			_ = entry.resource.ConfirmReleased(nil)
		}
		return nil, errors.Join(frame.ErrFenced, persisted)
	}
	b.executions[plan.Execution.ID] = entry
	session.revision++
	b.mu.Unlock()
	select {
	case result := <-entry.ready:
		return result.exports, result.err
	case <-ctx.Done():
		b.fence(entry)
		return nil, ctx.Err()
	}
}

func (b *Bridge) fence(entry *browserExecution) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session := b.sessions[string(RealmID(entry.plan.Realm))]
	if session == nil {
		return
	}
	fence := control.Fence{ScopeID: entry.plan.ScopeID, ScopeGeneration: entry.plan.ScopeGeneration}
	if _, exists := session.fences[fence]; !exists {
		session.fences[fence] = struct{}{}
		session.revision++
	}
}

func (b *Bridge) Poll(ctx context.Context, token string) (control.Poll, error) {
	session, err := b.session(token, false)
	if err != nil {
		return control.Poll{}, err
	}
	if err := ctx.Err(); err != nil {
		return control.Poll{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	result := control.Poll{Authority: b.authority, Realm: session.realm, Revision: strconv.FormatUint(session.revision, 10), Plans: []control.LocalPlan{}, Fences: []control.Fence{}}
	for _, entry := range b.executions {
		if entry.plan.Realm == session.realm && !entry.released && entry.readyOperation == "" && !session.cleanupOnly {
			entry.exposed = true
			result.Plans = append(result.Plans, clone(entry.plan))
		}
	}
	for fence := range session.fences {
		result.Fences = append(result.Fences, fence)
	}
	slices.SortFunc(result.Plans, func(a, b control.LocalPlan) int { return compare(a.Execution.ID, b.Execution.ID) })
	slices.SortFunc(result.Fences, func(a, b control.Fence) int { return compare(a.ScopeID, b.ScopeID) })
	return result, nil
}

func (b *Bridge) ValidatePlan(ctx context.Context, token string, plan control.LocalPlan) error {
	session, err := b.session(token, true)
	if err != nil {
		return err
	}
	b.mu.Lock()
	entry := b.executions[plan.Execution.ID]
	valid := entry != nil && !entry.released && entry.plan.Realm == session.realm && equal(entry.plan, plan)
	b.mu.Unlock()
	if !valid {
		return frame.ErrStaleBinding
	}
	return b.config.Host.Admission().Admit(ctx, func(admitted context.Context) error {
		return b.config.Host.ValidateBindingUnder(admitted, entry.attempt.Binding)
	})
}

func (b *Bridge) Ready(ctx context.Context, token string, report control.ReadyReport) (control.PublicationResult, error) {
	session, err := b.session(token, true)
	if err != nil {
		return control.PublicationResult{}, err
	}
	if report.Authority != b.authority || report.Realm != session.realm || report.OperationID == "" {
		return control.PublicationResult{Decision: control.Rejected, Problem: "wrong execution authority"}, nil
	}
	digest, err := requestDigest(report)
	if err != nil {
		return control.PublicationResult{}, err
	}
	b.mu.Lock()
	entry := b.executions[report.Execution.ID]
	if entry == nil || entry.plan.Execution != report.Execution || entry.plan.Realm != session.realm || entry.released {
		b.mu.Unlock()
		return control.PublicationResult{Decision: control.Rejected, Problem: "execution unavailable"}, nil
	}
	if entry.readyOperation != "" {
		same := entry.readyOperation == report.OperationID && entry.readyDigest == digest
		b.mu.Unlock()
		if !same {
			return control.PublicationResult{}, sqlite.ErrConflict
		}
		return control.PublicationResult{Decision: control.Accepted}, nil
	}
	b.mu.Unlock()
	if err := b.ValidatePlan(ctx, token, entry.plan); err != nil {
		return control.PublicationResult{Decision: control.Rejected, Problem: "plan is no longer current"}, nil
	}
	unit := entry.plan.Units[0]
	objects := map[string]control.ObjectRef{}
	metadata := map[string]string{}
	for _, contribution := range report.Contributions {
		if _, exists := metadata[contribution.Object.ObjectID]; exists {
			return control.PublicationResult{Decision: control.Rejected, Problem: "duplicate prepared object"}, nil
		}
		metadata[contribution.Object.ObjectID] = contribution.MetadataJSON
	}
	for _, reference := range report.Objects {
		if reference.ObjectID == "" || reference.Export.InstanceID != unit.Instance.ID {
			return control.PublicationResult{Decision: control.Rejected, Problem: "invalid object source"}, nil
		}
		if _, exists := objects[reference.Export.Name]; exists {
			return control.PublicationResult{Decision: control.Rejected, Problem: "duplicate export name"}, nil
		}
		objects[reference.Export.Name] = reference
	}
	exports := make([]frame.ExportValue, 0, len(report.Objects))
	for _, declaration := range unit.Exports {
		reference, exists := objects[declaration.Name]
		if !exists || reference.Export.Contract != declaration.Contract {
			return control.PublicationResult{Decision: control.Rejected, Problem: "actual exports differ from approved declarations"}, nil
		}
		var value any
		if declaration.Contract.Kind == frame.PointKind {
			encoded, exists := metadata[reference.ObjectID]
			if !exists {
				encoded = "null"
			}
			value, err = b.config.Host.DecodeMetadata(declaration.Contract, []byte(encoded))
			if err != nil {
				return control.PublicationResult{Decision: control.Rejected, Problem: "invalid point metadata"}, nil
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
			return control.PublicationResult{Decision: control.Rejected, Problem: "undeclared prepared contribution"}, nil
		}
		encoded, exists := metadata[reference.ObjectID]
		if !exists {
			return control.PublicationResult{Decision: control.Rejected, Problem: "missing prepared metadata"}, nil
		}
		value, err := b.config.Host.DecodeMetadata(reference.Export.Contract, []byte(encoded))
		if err != nil {
			return control.PublicationResult{Decision: control.Rejected, Problem: "invalid prepared metadata"}, nil
		}
		exports = append(exports, frame.ExportValue{Name: reference.Export.Name, Contract: reference.Export.Contract, Metadata: value, Value: Object{Reference: reference}})
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry.released {
		return control.PublicationResult{Decision: control.Rejected, Problem: "execution already released"}, nil
	}
	if entry.readyOperation != "" {
		if entry.readyOperation != report.OperationID || entry.readyDigest != digest {
			return control.PublicationResult{}, sqlite.ErrConflict
		}
		return control.PublicationResult{Decision: control.Accepted}, nil
	}
	entry.readyOperation = report.OperationID
	entry.readyDigest = digest
	entry.objects = slices.Clone(report.Objects)
	session.revision++
	entry.ready <- preparedResult{exports: exports}
	return control.PublicationResult{Decision: control.Accepted}, nil
}

func newToken() string {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(token[:])
}
func compare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
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
func requestDigest(value any) (integrity.Digest, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return integrity.Digest{}, err
	}
	canonical, err := jsonvalue.Parse(encoded)
	if err != nil {
		return integrity.Digest{}, err
	}
	return integrity.Hash(canonical.JSON()), nil
}
