package document

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sync"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	"github.com/Tangerg/frame/distribution"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type Config struct {
	MaxSubmissionsPerOwner int
	Database               string
	ContentDirectory       string
	DocumentsDirectory     string
}

const (
	fileInstallationID      frame.InstallationID = "files"
	converterInstallationID frame.InstallationID = "converter"
	validatorInstallationID frame.InstallationID = "validators"
	previewInstallationID   frame.InstallationID = "preview"
)

type App struct {
	Host       *frame.Host
	Store      *sqlite.Store
	Content    *distribution.Store
	Bridge     *browser.Bridge
	Contracts  Contracts
	owner      sqlite.Owner
	lifetime   *frame.Lifetime
	scope      frame.ScopeGrant
	preview    distribution.Release
	recoveryMu sync.Mutex
	mu         sync.Mutex
	runs       map[string]*taskRun
}

func Open(ctx context.Context, config Config) (_ *App, err error) {
	if config.Database == "" || config.ContentDirectory == "" || config.DocumentsDirectory == "" {
		return nil, errors.New("document: database, content and document directories are required")
	}
	contracts, err := NewContracts()
	if err != nil {
		return nil, err
	}
	admission := frame.NewAdmission()
	store, err := sqlite.Open(ctx, sqlite.Config{Path: config.Database, Admission: admission, Owners: []sqlite.OwnerSchema{taskSchema()}, MaxRequestBytes: maximumTaskBytes, MaxSubmissionsPerOwner: config.MaxSubmissionsPerOwner})
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, store.Close())
		}
	}()
	owner, err := store.Owner(taskOwnerID)
	if err != nil {
		return nil, err
	}
	lifetime := frame.NewLifetime(context.Background())
	a := &App{Store: store, Contracts: contracts, owner: owner, lifetime: lifetime, runs: make(map[string]*taskRun)}
	host, err := frame.NewHost(context.Background(), frame.HostConfig{ReadAcceptedBinding: a.readQueuedBinding, Admission: admission, Lifetime: lifetime, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store, UseOwners: []frame.UseOwner{owner}})
	if err != nil {
		return nil, err
	}
	a.Host = host
	var trust previewTrust
	content, err := distribution.Open(distribution.Config{Directory: config.ContentDirectory, Admission: admission, Trust: trust.check, Retained: a.retainsRelease})
	if err != nil {
		return nil, err
	}
	a.Content = content
	defer func() {
		if err != nil {
			outcome, closeErr := host.Close(context.WithoutCancel(ctx))
			err = errors.Join(err, closeErr)
			if outcome.Released {
				err = errors.Join(err, content.Close(context.WithoutCancel(ctx)))
			}
		}
	}()
	resources, err := distribution.NewResourceDriver(content)
	if err != nil {
		return nil, err
	}
	if err := host.Register(contracts.Files, contracts.Converters, contracts.Validators, contracts.Previews, contracts.Theme); err != nil {
		return nil, err
	}
	if err := host.RegisterDriver(resources); err != nil {
		return nil, err
	}
	bridge, err := browser.NewBridge(browser.BridgeConfig{Host: host, Store: store, Source: moduleSource{store: content}, Points: []frame.ContractRef{contracts.Previews.Ref()}})
	if err != nil {
		return nil, err
	}
	a.Bridge = bridge
	if err := host.RegisterDriver(bridge); err != nil {
		return nil, err
	}
	if err := a.restore(ctx, config.DocumentsDirectory, trust.pin); err != nil {
		return nil, err
	}
	return a, nil
}

type previewTrust struct{ archive value.Digest }

func (t *previewTrust) pin(archive value.Digest) { t.archive = archive }

func (t *previewTrust) check(_ context.Context, source distribution.Source, manifest distribution.Manifest, _ distribution.Lock) error {
	if t.archive.IsZero() || source.ArchiveDigest != t.archive || manifest.Package != previewPackageName || manifest.Version != previewPackageVersion {
		return distribution.ErrUntrusted
	}
	return nil
}

// retainsRelease reports a release still installed or still protecting an
// execution, whose content must therefore stay.
func (a *App) retainsRelease(ctx context.Context, release value.Digest) (bool, error) {
	records, err := a.Store.Installations(ctx)
	if err != nil {
		return false, err
	}
	if slices.ContainsFunc(records, func(record frame.InstallationRecord) bool { return record.Release == release }) {
		return true, nil
	}
	dependencies, err := a.Store.ExecutionDependencies(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(dependencies, func(dependency frame.Dependency) bool { return dependency.Release == release }), nil
}

type catalog struct {
	files, converter, secondConverter, validators *frame.Plugin
}

func newCatalog(contracts Contracts) (catalog, error) {
	files, err := FilePlugin(contracts)
	if err != nil {
		return catalog{}, err
	}
	converter, err := ConverterPlugin(contracts, "1")
	if err != nil {
		return catalog{}, err
	}
	second, err := ConverterPlugin(contracts, "2")
	if err != nil {
		return catalog{}, err
	}
	validators, err := ValidationPlugin(contracts)
	return catalog{files: files, converter: converter, secondConverter: second, validators: validators}, err
}

func (c catalog) definitions() []*frame.Plugin {
	return []*frame.Plugin{c.files, c.converter, c.secondConverter, c.validators}
}

func (a *App) restore(ctx context.Context, directory string, trustPreview func(value.Digest)) error {
	records, err := a.Store.RecoverInstallations(ctx)
	if err != nil {
		return err
	}
	if err := a.owner.RecoverPrepared(ctx); err != nil {
		return err
	}
	initialized, err := a.initialized(ctx)
	if err != nil {
		return err
	}
	plugins, err := newCatalog(a.Contracts)
	if err != nil {
		return err
	}
	preview, err := a.restorePreview(ctx, records, initialized, trustPreview)
	if err != nil {
		return err
	}
	definitions := plugins.definitions()
	if preview != nil {
		definitions = append(definitions, preview)
	}
	if err := a.Host.Restore(ctx, records, definitions); err != nil {
		return err
	}
	if err := a.Store.ConfirmRestoredInstallations(ctx); err != nil {
		return err
	}
	if err := a.Recover(ctx); err != nil {
		if initialized && errors.Is(err, frame.ErrRecoveryRequired) {
			return nil
		}
		return err
	}
	if initialized {
		return nil
	}
	return a.bootstrap(ctx, directory, plugins, preview)
}

func (a *App) initialized(ctx context.Context) (bool, error) {
	var initialized bool
	err := a.Store.Read(ctx, func(tx sqlite.Reader) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM document_bootstrap)`).Scan(&initialized)
	})
	return initialized, err
}

func (a *App) restorePreview(ctx context.Context, records []frame.InstallationRecord, initialized bool, trustPreview func(value.Digest)) (*frame.Plugin, error) {
	var err error
	for _, record := range records {
		if record.ID == previewInstallationID {
			if a.preview, err = a.Content.Inspect(ctx, record.Release); err != nil {
				return nil, err
			}
		}
	}
	if a.preview.Digest.IsZero() && !initialized {
		if a.preview, err = buildPreview(ctx, a.Content, a.Contracts, trustPreview); err != nil {
			return nil, err
		}
	}
	if a.preview.Digest.IsZero() {
		return nil, nil
	}
	return previewDefinition(a.preview)
}

// bootstrap installs the initial plugins once; the bootstrap row records that
// a restart must restore rather than install them again.
func (a *App) bootstrap(ctx context.Context, directory string, plugins catalog, preview *frame.Plugin) error {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	configuration, err := json.Marshal(FileConfiguration{Directory: directory})
	if err != nil {
		return err
	}
	for _, installation := range []frame.InstallationConfig{
		{ID: fileInstallationID, Plugin: plugins.files, Configuration: configuration, Enabled: true},
		{ID: converterInstallationID, Plugin: plugins.converter, Enabled: true},
		{ID: validatorInstallationID, Plugin: plugins.validators, Enabled: true},
		{ID: previewInstallationID, Plugin: preview, Enabled: true},
	} {
		if _, installed := a.Host.Installation(installation.ID); installed {
			continue
		}
		if _, err := a.Host.Install(ctx, installation); err != nil {
			return err
		}
	}
	return a.owner.Update(ctx, func(ctx context.Context, tx sqlite.Executor) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO document_bootstrap(singleton) VALUES(1)`)
		return err
	})
}

// Recover does not replay a task that reached unsettled. Such a task remains an
// application-owned unknown result until the user explicitly cancels it. Old
// surviving Realms keep recovery closed until their original proofs arrive.
func (a *App) Recover(ctx context.Context) error {
	a.recoveryMu.Lock()
	defer a.recoveryMu.Unlock()
	if a.currentScope() != (frame.ScopeGrant{}) {
		return a.Host.Admission().RecoveryError()
	}
	dependencies, err := a.Store.ExecutionDependencies(ctx)
	if err != nil {
		return err
	}
	if len(dependencies) != 0 {
		return frame.ErrRecoveryRequired
	}
	if err := a.Host.Admission().Recover(ctx, func() error { return nil }); err != nil {
		return err
	}
	scope, err := a.Host.Admission().OpenScope(ctx, "document:tasks")
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.scope = scope
	a.mu.Unlock()
	return nil
}

func (a *App) currentScope() frame.ScopeGrant { a.mu.Lock(); defer a.mu.Unlock(); return a.scope }

func (a *App) activateDocumentProviders(ctx context.Context) error {
	records, err := a.Store.Installations(ctx)
	if err != nil {
		return err
	}
	var roots []frame.ExportRef
	for _, record := range records {
		if !record.Enabled {
			continue
		}
		for _, unit := range record.Units {
			for _, export := range unit.Exports {
				if export.Contract == a.Contracts.Converters.Ref() || export.Contract == a.Contracts.Validators.Ref() {
					roots = append(roots, frame.ExportRef{InstallationID: record.ID, Release: record.Release, UnitID: unit.ID, Export: export.Name, Contract: export.Contract})
				}
			}
		}
	}
	_, err = a.Host.Activate(ctx, roots...)
	return err
}

func (a *App) plan(ctx context.Context) (*frame.BindingPlan, error) {
	if err := a.activateDocumentProviders(ctx); err != nil {
		return nil, err
	}
	snapshot, err := a.Host.Snapshot(ctx, a.Contracts.Converters.Ref(), a.Contracts.Validators.Ref())
	if err != nil {
		return nil, err
	}
	converter, err := frame.SelectOne(snapshot, a.Contracts.Converters, "documents.markdown-priority/1", selectMarkdownConverter)
	if err != nil {
		return nil, err
	}
	validation, err := frame.SelectMany(snapshot, a.Contracts.Validators, "documents.validation-order/1", orderValidators)
	if err != nil {
		return nil, err
	}
	return frame.Capture(snapshot, converter.Ref(), validation.Ref())
}

func selectMarkdownConverter(values []frame.Candidate[ConverterMetadata, Converter]) (frame.Candidate[ConverterMetadata, Converter], error) {
	type converterCandidate struct {
		candidate frame.Candidate[ConverterMetadata, Converter]
		metadata  ConverterMetadata
	}
	converters := make([]converterCandidate, 0, len(values))
	for _, candidate := range values {
		metadata, err := candidate.Metadata()
		if err != nil {
			return frame.Candidate[ConverterMetadata, Converter]{}, err
		}
		if metadata.Format == markdownFormat {
			converters = append(converters, converterCandidate{candidate, metadata})
		}
	}
	if len(converters) == 0 {
		return frame.Candidate[ConverterMetadata, Converter]{}, frame.ErrUnavailable
	}
	selected := slices.MinFunc(converters, func(left, right converterCandidate) int {
		return cmp.Or(cmp.Compare(right.metadata.Priority, left.metadata.Priority), cmp.Compare(left.candidate.Source().InstallationID, right.candidate.Source().InstallationID))
	})
	return selected.candidate, nil
}

func orderValidators(values []frame.Candidate[ValidatorMetadata, Validator]) ([]frame.Candidate[ValidatorMetadata, Validator], error) {
	type validatorCandidate struct {
		candidate frame.Candidate[ValidatorMetadata, Validator]
		metadata  ValidatorMetadata
	}
	validators := make([]validatorCandidate, len(values))
	for index, candidate := range values {
		metadata, err := candidate.Metadata()
		if err != nil {
			return nil, err
		}
		validators[index] = validatorCandidate{candidate, metadata}
	}
	slices.SortFunc(validators, func(left, right validatorCandidate) int {
		return cmp.Or(cmp.Compare(left.metadata.Order, right.metadata.Order), cmp.Compare(left.metadata.Name, right.metadata.Name))
	})
	for index, validator := range validators {
		values[index] = validator.candidate
	}
	return values, nil
}

func (a *App) ReplaceConverter(ctx context.Context, version string) error {
	if version != "1" && version != "2" {
		return errors.New("document: unknown built-in converter revision")
	}
	plugin, err := ConverterPlugin(a.Contracts, version)
	if err != nil {
		return err
	}
	return a.Host.Replace(ctx, converterInstallationID, plugin, nil)
}

func (a *App) Theme(ctx context.Context) (string, error) {
	if a.preview.Digest.IsZero() {
		return "", frame.ErrUnavailable
	}
	root := frame.ExportRef{InstallationID: previewInstallationID, Release: a.preview.Digest, UnitID: themeUnitID, Export: themeExportName, Contract: a.Contracts.Theme.Ref()}
	plan, err := a.Host.Plan(ctx, root)
	if err != nil {
		return "", err
	}
	use, err := a.Host.Acquire(ctx, plan, frame.UseConfig{Owner: a.lifetime, Scope: a.currentScope()})
	if err != nil {
		return "", err
	}
	var text string
	err = frame.BorrowExport(ctx, use, root, func(ctx context.Context, resource *distribution.Resources) error {
		data, err := resource.Read(ctx, themeResourcePath)
		text = string(data)
		return err
	})
	_, closeErr := use.Close(context.WithoutCancel(ctx))
	return text, errors.Join(err, closeErr)
}

func (a *App) ActivatePreview(ctx context.Context, credentials browser.Credentials) error {
	if a.preview.Digest.IsZero() {
		return frame.ErrUnavailable
	}
	poll, err := a.Bridge.Poll(ctx, credentials.Token)
	if err != nil {
		return err
	}
	if poll.Realm != credentials.Realm {
		return frame.ErrFenced
	}
	root := frame.ExportRef{InstallationID: previewInstallationID, Release: a.preview.Digest, UnitID: previewUnitID, Export: previewExportName, Contract: a.Contracts.Previews.Ref()}
	_, err = a.Host.ActivateIn(ctx, browser.RealmID(credentials.Realm), root)
	return err
}

func (a *App) Close(ctx context.Context) error {
	settlementErr := a.settlePendingTasks(ctx)
	outcome, err := a.Host.Close(ctx)
	err = errors.Join(settlementErr, err)
	if !outcome.Released {
		return errors.Join(frame.ErrUnconfirmedRelease, err)
	}
	if contentErr := a.Content.Close(ctx); contentErr != nil {
		return errors.Join(err, contentErr)
	}
	return errors.Join(err, a.Store.Close())
}
