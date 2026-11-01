package document

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"

	"github.com/Tangerg/frame"
	controlhost "github.com/Tangerg/frame/control/host"
	"github.com/Tangerg/frame/distribution"
	"github.com/Tangerg/frame/integrity"
	"github.com/Tangerg/frame/sqlite"
)

type Config struct {
	Database           string
	ContentDirectory   string
	DocumentsDirectory string
}

type App struct {
	Host          *frame.Host
	Store         *sqlite.Store
	Content       *distribution.Store
	Bridge        *controlhost.Bridge
	Contracts     Contracts
	owner         *sqlite.Owner
	lifetime      *frame.Lifetime
	scope         frame.ScopeGrant
	preview       distribution.Release
	plugins       []*frame.Plugin
	mu            sync.Mutex
	running       map[string]bool
	reconstructed bool
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
	store, err := sqlite.Open(ctx, sqlite.Config{Path: config.Database, Admission: admission, Owners: []sqlite.OwnerSchema{taskSchema()}, MaxRequestBytes: 32 << 20})
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, store.Close())
		}
	}()
	owner, err := store.Owner("document.tasks")
	if err != nil {
		return nil, err
	}
	lifetime := frame.NewLifetime(context.Background())
	a := &App{Store: store, Contracts: contracts, owner: owner, lifetime: lifetime, running: map[string]bool{}, reconstructed: true}
	host, err := frame.NewHost(context.Background(), frame.HostConfig{ReadAcceptedBinding: a.readQueuedBinding, Admission: admission, Lifetime: lifetime, Epoch: store.Epoch(), Installations: store, ExecutionProtection: store, UseOwners: []frame.UseOwner{owner}})
	if err != nil {
		return nil, err
	}
	a.Host = host
	var trustedArchive integrity.Digest
	content, err := distribution.Open(distribution.Config{Directory: config.ContentDirectory, Admission: admission, Trust: func(_ context.Context, source distribution.Source, manifest distribution.Manifest, _ distribution.Lock) error {
		if trustedArchive.IsZero() || source.ArchiveDigest != trustedArchive || manifest.Package != "example.document-preview" || manifest.Version != "1" {
			return distribution.ErrUntrusted
		}
		return nil
	}, Retained: func(ctx context.Context, release integrity.Digest) (bool, error) {
		records, err := store.Installations(ctx)
		if err != nil {
			return false, err
		}
		for _, record := range records {
			if record.Release == release {
				return true, nil
			}
		}
		dependencies, err := store.ExecutionDependencies(ctx)
		if err != nil {
			return false, err
		}
		for _, dependency := range dependencies {
			if dependency.Release == release {
				return true, nil
			}
		}
		return false, nil
	}})
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
	bridge, err := controlhost.NewBridge(controlhost.BridgeConfig{Host: host, Store: store, Source: moduleSource{store: content}, Points: []frame.ContractRef{contracts.Previews.Ref()}})
	if err != nil {
		return nil, err
	}
	a.Bridge = bridge
	if err := host.RegisterDriver(bridge); err != nil {
		return nil, err
	}
	files, err := FilePlugin(contracts)
	if err != nil {
		return nil, err
	}
	converter, err := ConverterPlugin(contracts, "1")
	if err != nil {
		return nil, err
	}
	second, err := ConverterPlugin(contracts, "2")
	if err != nil {
		return nil, err
	}
	validators, err := ValidationPlugin(contracts)
	if err != nil {
		return nil, err
	}
	records, err := store.RecoverInstallations(ctx)
	if err != nil {
		return nil, err
	}
	if err := owner.RecoverPrepared(ctx); err != nil {
		return nil, err
	}
	if len(records) == 0 {
		// This distribution is built from files embedded in this executable.
		// Remote or user-authored packages require their own trust decision.
		a.preview, err = buildPreview(ctx, content, contracts, func(digest integrity.Digest) { trustedArchive = digest })
		if err != nil {
			return nil, err
		}
	} else {
		for _, record := range records {
			if record.ID == "preview" {
				a.preview, err = content.Inspect(ctx, record.Release)
				if err != nil {
					return nil, err
				}
			}
		}
		if a.preview.Digest.IsZero() {
			return nil, errors.New("document: retained preview release is missing")
		}
	}
	preview, err := previewDefinition(a.preview)
	if err != nil {
		return nil, err
	}
	a.plugins = []*frame.Plugin{files, converter, second, validators, preview}
	if len(records) == 0 {
		directory, err := filepath.Abs(config.DocumentsDirectory)
		if err != nil {
			return nil, err
		}
		configuration, _ := json.Marshal(FileConfiguration{Directory: directory})
		for _, installation := range []frame.InstallationConfig{{ID: "files", Plugin: files, Configuration: configuration, Enabled: true}, {ID: "converter", Plugin: converter, Enabled: true}, {ID: "validators", Plugin: validators, Enabled: true}, {ID: "preview", Plugin: preview, Enabled: true}} {
			if _, err := host.Install(ctx, installation); err != nil {
				return nil, err
			}
		}
	} else {
		if err := host.Restore(ctx, records, a.plugins); err != nil {
			return nil, err
		}
		if err := store.ConfirmRestoredInstallations(ctx); err != nil {
			return nil, err
		}
	}
	if err := a.Recover(ctx); err != nil && !errors.Is(err, frame.ErrRecoveryRequired) {
		return nil, err
	}
	return a, nil
}

// Recover does not replay a task that reached running. Such a task remains an
// application-owned unknown result until the user explicitly cancels it. Old
// surviving Realms keep recovery closed until their original proofs arrive.
func (a *App) Recover(ctx context.Context) error {
	a.mu.Lock()
	reconstructed := a.reconstructed
	a.mu.Unlock()
	if !reconstructed {
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
	a.reconstructed = false
	a.mu.Unlock()
	return nil
}

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
	converters, err := frame.Candidates(snapshot, a.Contracts.Converters)
	if err != nil {
		return nil, err
	}
	converters = slices.DeleteFunc(converters, func(candidate frame.Candidate[ConverterMetadata, Converter]) bool {
		return candidate.Metadata().Format != "markdown"
	})
	slices.SortFunc(converters, func(left, right frame.Candidate[ConverterMetadata, Converter]) int {
		return cmp.Or(cmp.Compare(right.Metadata().Priority, left.Metadata().Priority), cmp.Compare(left.Source().InstallationID, right.Source().InstallationID))
	})
	if len(converters) == 0 {
		return nil, frame.ErrUnavailable
	}
	validators, err := frame.Candidates(snapshot, a.Contracts.Validators)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(validators, func(left, right frame.Candidate[ValidatorMetadata, Validator]) int {
		return cmp.Or(cmp.Compare(left.Metadata().Order, right.Metadata().Order), cmp.Compare(left.Metadata().Name, right.Metadata().Name))
	})
	converter, err := frame.Select(snapshot, a.Contracts.Converters, "documents.markdown-priority/1", converters[0])
	if err != nil {
		return nil, err
	}
	validation, err := frame.Select(snapshot, a.Contracts.Validators, "documents.validation-order/1", validators...)
	if err != nil {
		return nil, err
	}
	return frame.Capture(snapshot, converter.Ref(), validation.Ref())
}

func (a *App) ReplaceConverter(ctx context.Context, version string) error {
	if version != "1" && version != "2" {
		return errors.New("document: unknown built-in converter revision")
	}
	plugin, err := ConverterPlugin(a.Contracts, version)
	if err != nil {
		return err
	}
	return a.Host.Replace(ctx, "converter", plugin, nil)
}

func (a *App) Theme(ctx context.Context) (string, error) {
	root := frame.ExportRef{InstallationID: "preview", Release: a.preview.Digest, UnitID: "theme", Export: "theme", Contract: a.Contracts.Theme.Ref()}
	plan, err := a.Host.Plan(ctx, root)
	if err != nil {
		return "", err
	}
	use, err := a.Host.Acquire(ctx, plan, frame.UseConfig{Owner: a.lifetime, Scope: a.scope})
	if err != nil {
		return "", err
	}
	var text string
	err = frame.BorrowExport(ctx, use, root, func(ctx context.Context, resource *distribution.Resources) error {
		data, err := resource.Read(ctx, "theme.css")
		text = string(data)
		return err
	})
	_, closeErr := use.Close(context.WithoutCancel(ctx))
	return text, errors.Join(err, closeErr)
}

func (a *App) ActivatePreview(ctx context.Context, credentials controlhost.Credentials) error {
	poll, err := a.Bridge.Poll(ctx, credentials.Token)
	if err != nil {
		return err
	}
	if poll.Realm != credentials.Realm {
		return frame.ErrFenced
	}
	root := frame.ExportRef{InstallationID: "preview", Release: a.preview.Digest, UnitID: "preview", Export: "preview", Contract: a.Contracts.Previews.Ref()}
	_, err = a.Host.ActivateIn(ctx, controlhost.RealmID(credentials.Realm), root)
	return err
}

func (a *App) Close(ctx context.Context) error {
	outcome, err := a.Host.Close(ctx)
	if !outcome.Released {
		return errors.Join(frame.ErrUnconfirmedRelease, err)
	}
	if contentErr := a.Content.Close(ctx); contentErr != nil {
		return errors.Join(err, contentErr)
	}
	return errors.Join(err, a.Store.Close())
}

func (a *App) String() string { return fmt.Sprintf("document workbench (%s)", a.Store.ID()) }
