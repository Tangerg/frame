package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/process"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

const ManagedProfile framehost.Profile = "qimu-mcp"

// ManagedProtocol resolves physical exports from the package already approved
// by the host. The map is keyed by the logical export name in the Frame unit.
type ManagedProtocol struct {
	Exports       func(framehost.UnitBinding) (map[string]ManagedExport, error)
	OpenCallbacks OpenManagedCallbacks
	OpenCall      OpenManagedCall
	OpenStream    OpenManagedStream
}

type ManagedExport struct {
	Name       string
	Contract   *ManagedContract
	Requires   []ManagedImport
	Background bool
}

func (ManagedProtocol) Profile() framehost.Profile { return ManagedProfile }

func (p ManagedProtocol) Prepare(setup *framehost.Setup, metadata process.ExportMetadata, maxBytes int) (process.Connection, error) {
	if p.Exports == nil || setup == nil {
		return nil, frame.ErrInvalid
	}
	unit := setup.Unit()
	if len(unit.Imports) != 0 && p.OpenCallbacks == nil {
		return nil, fmt.Errorf("%w: managed dependencies require a reverse callback grant", frame.ErrContractMismatch)
	}
	declared, err := p.Exports(setup.Binding())
	if err != nil {
		return nil, err
	}
	if len(unit.Exports) != len(declared) {
		return nil, fmt.Errorf("%w: managed export declarations differ", frame.ErrContractMismatch)
	}
	prepared := &managedConnection{openStream: p.OpenStream, openCall: p.OpenCall, setup: setup, contributions: make([]managedContribution, 0, len(declared)), maxBytes: maxBytes, onActivate: setup.OnActivate}
	builder := managedBuilder{protocol: p, setup: setup, connection: prepared, imports: make(map[string]framehost.ImportDeclaration, len(unit.Imports)), used: make(map[string]bool, len(unit.Imports)), routes: make(managedRoutes)}
	for _, slot := range unit.Imports {
		builder.imports[slot.Slot] = slot
	}
	for _, export := range unit.Exports {
		if err := builder.add(export, declared[export.Name], metadata); err != nil {
			return nil, err
		}
	}
	if len(builder.used) != len(builder.imports) {
		return nil, fmt.Errorf("%w: unowned managed import", frame.ErrContractMismatch)
	}
	return prepared, nil
}

// managedBuilder projects each approved Frame export onto the physical export
// the managed package declares for it. Every Frame import must be required
// by exactly the contributions that use it.
type managedBuilder struct {
	protocol   ManagedProtocol
	setup      *framehost.Setup
	connection *managedConnection
	imports    map[string]framehost.ImportDeclaration
	used       map[string]bool
	routes     managedRoutes
}

func (b *managedBuilder) add(export framehost.ExportDeclaration, source ManagedExport, metadata process.ExportMetadata) error {
	if source.Contract == nil || source.Contract.capability == nil || !managedExportName.MatchString(source.Name) {
		return fmt.Errorf("%w: missing managed export %q", frame.ErrContractMismatch, export.Name)
	}
	source.Contract = new(*source.Contract)
	if err := b.checkContract(export, source); err != nil {
		return err
	}
	if err := b.connection.catalog.admit(export.Name, source.Name, source.Contract); err != nil {
		return err
	}
	dependencies, err := projectDependencies(b.setup, b.imports, b.used, b.routes, source.Requires)
	if err != nil {
		return err
	}
	if source.Background || len(dependencies) > 0 {
		if b.protocol.OpenCallbacks == nil {
			return fmt.Errorf("%w: managed contribution requires a host callback lease", frame.ErrContractMismatch)
		}
		b.connection.openCallbacks = b.protocol.OpenCallbacks
	}
	contribution := managedContribution{ID: export.Name, Export: source.Name, Requires: dependencies}
	if source.Background {
		contribution.Background = installationTarget(b.setup)
	}
	b.connection.contributions = append(b.connection.contributions, contribution)
	b.connection.values = append(b.connection.values, metadata.Export(export, nil))
	return nil
}

// The managed projection cannot weaken the host's approved capability or
// publish a stream without its reverse-delivery authority.
func (b *managedBuilder) checkContract(export framehost.ExportDeclaration, source ManagedExport) error {
	capability, err := b.setup.Capability(export.Contract)
	if err != nil {
		return err
	}
	if capability.Identity() != source.Contract.capability.Identity() {
		return frame.ErrContractMismatch
	}
	for _, member := range capability.Descriptor().Methods {
		if len(member.Stream) != 0 && b.protocol.OpenStream == nil {
			return fmt.Errorf("%w: managed streaming requires reverse delivery", frame.ErrContractMismatch)
		}
	}
	return nil
}

type managedConnection struct {
	openCall      OpenManagedCall
	openStream    OpenManagedStream
	openCallbacks OpenManagedCallbacks
	setup         *framehost.Setup
	maxBytes      int
	onActivate    func(func(context.Context) error) error
	catalog       managedCatalog
	contributions []managedContribution
	values        []framehost.ExportValue
	session       *sdk.ClientSession
}

func (c *managedConnection) Connect(ctx context.Context, channel io.ReadWriteCloser) ([]framehost.ExportValue, error) {
	session, err := connect(ctx, channel, c.maxBytes, registerManaged)
	if err != nil {
		return nil, err
	}
	c.session = session
	var description managedDescription
	if err := managedCall(ctx, session, managedDescribe, &managedParams{Protocol: managedVersion}, &description); err != nil {
		return nil, err
	}
	receipt, err := c.catalog.verify(description)
	if err != nil {
		return nil, err
	}
	setup := &managedParams{Protocol: managedVersion, Contributions: &c.contributions}
	if c.openCallbacks != nil {
		lease, err := reserveManagedLease(c.setup.Lifetime())
		if err != nil {
			return nil, err
		}
		grant, err := lease.issue(func() (*ManagedCallback, error) { return issuedGrant(c.openCallbacks(ctx, c.setup, session)) })
		if err == nil && grant == nil {
			err = errors.New("mcp: missing managed setup callback grant")
		}
		if err != nil {
			return nil, errors.Join(err, lease.finish(ctx))
		}
		if err := grant.attach(&setup.ParamsBase, managedCallback); err != nil {
			return nil, errors.Join(err, lease.finish(ctx))
		}

	}
	if err := managedAcknowledge(ctx, session, managedSetup, setup); err != nil {
		return nil, err
	}
	if err := c.onActivate(func(ctx context.Context) error {
		return managedAcknowledge(ctx, session, managedActivate, &managedParams{Protocol: managedVersion})
	}); err != nil {
		return nil, err
	}
	return c.bindExports(c.setup.Binding().Source, c.setup.Lifetime(), receipt), nil
}

func (c *managedConnection) bindExports(unit framehost.UnitRef, lifetime frame.Lifetime, receipt *frame.Method) []framehost.ExportValue {
	for i, contribution := range c.contributions {
		published := &c.values[i]
		source := framehost.ExportRef{InstallationID: unit.InstallationID, Release: unit.Release, UnitID: unit.UnitID, Export: published.Name, Contract: published.Contract}
		published.Value = &ManagedService{
			session: c.session, source: source, physical: contribution.Export, contract: c.catalog.exports[contribution.Export],
			receipt: receipt, openStream: c.openStream, openCall: c.openCall, lifetime: lifetime,
		}
	}
	return c.values
}

func (c *managedConnection) Close(context.Context) error {
	if c.session == nil {
		return nil
	}
	return c.session.Close()
}

func managedAcknowledge(ctx context.Context, session *sdk.ClientSession, name string, params *managedParams) error {
	var ok bool
	if err := managedCall(ctx, session, name, params, &ok); err != nil {
		return err
	}
	if !ok {
		return errors.New("mcp: managed backend declined " + name)
	}
	return nil
}
