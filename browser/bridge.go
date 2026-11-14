package browser

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	framevalue "github.com/Tangerg/frame/value"
)

type ModuleSource interface {
	Module(context.Context, framehost.UnitBinding) (Artifact, error)
}

// RemoteSource binds a declared Portable transport to an exact approved import.
// The adapter registers all sessions and call resources with the original Setup
// before returning. A descriptor alone never creates a callable transport.
type RemoteSource interface {
	Import(context.Context, framehost.Setup, framehost.BoundValue) (RemoteImport, error)
}

type BridgeConfig struct {
	Host   framehost.Host
	Store  sqlite.Store
	Source ModuleSource
	Remote RemoteSource
	// PublicConfiguration is the consumer's explicit public projection. Opaque
	// installation configuration may contain credentials and is never sent by
	// default. The instance keeps the original configuration digest as identity.
	PublicConfiguration func(context.Context, framehost.Setup) ([]byte, error)
	MaxRealms           int
	MaxPending          int
	MaxMessageBytes     int64
	Points              []framehost.ContractRef
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

// Bridge is the browser-js driver and the Host side of every Realm's control
// channel. Its tables project Host and SQLite owners for one authority epoch.
type Bridge struct {
	config    BridgeConfig
	authority Authority
	signer    realmSigner

	// mu guards every table below and the mutable state of their entries.
	mu         sync.Mutex
	openings   map[string]*realmOpening
	sessions   boundedTable[framehost.RealmID, *realmSession]
	executions boundedTable[string, *browserExecution]
	// operations are live control operations; receiptReads join concurrent
	// reads of an archived one.
	operations   map[string]*controlOperation
	receiptReads map[string]*receiptRead
	publications map[string]*controlOperation
	snapshots    map[string]*snapshotLease
	uses         map[string]*browserUse
	routes       boundedTable[string, browserRoute]
}

// browserUse is a Use granted to a Realm. releasedBy names the exit proof
// that released it; until then the Realm may still use and delegate it.
type browserUse struct {
	grant      UseGrant
	use        framehost.Use
	resource   frame.ResourceHandle
	releasedBy string
}

func (u *browserUse) released() bool { return u.releasedBy != "" }

type browserRoute struct {
	grant    RouteGrant
	original framehost.EventRoute
}

func NewBridge(config BridgeConfig) (*Bridge, error) {
	if !config.Host.Valid() || framevalue.IsNil(config.Source) || config.Host.Durable() != framehost.Durable(config.Store) {
		return nil, frame.ErrInvalid
	}
	if config.Remote != nil && framevalue.IsNil(config.Remote) {
		return nil, frame.ErrInvalid
	}
	config.MaxRealms = cmp.Or(config.MaxRealms, defaultMaxRealms)
	config.MaxPending = cmp.Or(config.MaxPending, defaultMaxPending)
	config.MaxMessageBytes = cmp.Or(config.MaxMessageBytes, defaultMaxMessageBytes)
	if config.MaxRealms < 1 || config.MaxPending < 1 || config.MaxMessageBytes < 1 {
		return nil, frame.ErrInvalid
	}
	config.Points = slices.Clone(config.Points)
	if err := validateConfiguredPoints(config.Points); err != nil {
		return nil, err
	}
	return &Bridge{
		config:       config,
		authority:    Authority{ID: config.Store.ID(), Epoch: strconv.FormatUint(config.Store.Epoch(), 10)},
		signer:       newRealmSigner(),
		openings:     make(map[string]*realmOpening),
		sessions:     newBoundedTable[framehost.RealmID, *realmSession]("realms", config.MaxRealms),
		executions:   newBoundedTable[string, *browserExecution]("pending executions", config.MaxPending),
		operations:   make(map[string]*controlOperation),
		receiptReads: make(map[string]*receiptRead),
		publications: make(map[string]*controlOperation),
		snapshots:    make(map[string]*snapshotLease),
		uses:         make(map[string]*browserUse),
		routes:       newBoundedTable[string, browserRoute]("event routes", config.MaxPending),
	}, nil
}

func validateConfiguredPoints(points []framehost.ContractRef) error {
	names := make(map[string]struct{}, len(points))
	for _, point := range points {
		if err := point.Validate(); err != nil {
			return fmt.Errorf("browser: configured point: %w", err)
		}
		if point.Kind != framehost.PointKind {
			return fmt.Errorf("%w: browser configured point requires a point contract", frame.ErrInvalid)
		}
		if _, duplicate := names[point.Name]; duplicate {
			return fmt.Errorf("%w: duplicate browser point name %q", frame.ErrInvalid, point.Name)
		}
		names[point.Name] = struct{}{}
	}
	return nil
}

func (*Bridge) Profile() framehost.Profile { return framehost.BrowserJS }
func (*Bridge) RealmBound() bool           { return true }
func (b *Bridge) Authority() Authority     { return b.authority }

const useResourcePrefix = "browser use "

// jsonNull is the wire spelling of absent Point metadata.
const jsonNull = "null"
