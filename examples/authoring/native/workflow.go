package native

import (
	"cmp"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"os"
	"slices"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/document"
	framehost "github.com/Tangerg/frame/host"
)

const Installation framehost.InstallationID = "authored-document"

type Workbench struct {
	Host      framehost.Host
	Contracts document.Contracts
	scope     framehost.ScopeGrant
	output    string
}

func Open(ctx context.Context, config Configuration, outputDirectory string) (_ *Workbench, err error) {
	if outputDirectory == "" {
		return nil, errors.New("authoring: output directory is required")
	}
	contracts, err := document.NewContracts()
	if err != nil {
		return nil, err
	}
	host, err := framehost.New(ctx, framehost.Config{})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_, closeErr := host.Close(context.WithoutCancel(ctx))
			err = errors.Join(err, closeErr)
		}
	}()
	if err := host.Register(contracts.Files, contracts.Converters, contracts.Validators); err != nil {
		return nil, err
	}
	plugin, err := Plugin(contracts)
	if err != nil {
		return nil, err
	}
	configuration, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	installed, err := host.Install(ctx, framehost.InstallationConfig{ID: Installation, Plugin: plugin, Configuration: configuration, Enabled: true})
	if err != nil {
		return nil, err
	}
	var roots []framehost.ExportRef
	for _, unit := range plugin.Units() {
		for _, export := range unit.Exports {
			roots = append(roots, framehost.ExportRef{InstallationID: installed.ID, Release: installed.Release, UnitID: unit.ID, Export: export.Name, Contract: export.Contract})
		}
	}
	if _, err := host.Activate(ctx, roots...); err != nil {
		return nil, err
	}
	scope, err := host.Admission().OpenScope(ctx, "authoring:documents")
	if err != nil {
		return nil, err
	}
	return &Workbench{Host: host, Contracts: contracts, scope: scope, output: outputDirectory}, nil
}

func (w *Workbench) Close(ctx context.Context) error {
	_, err := w.Host.Close(ctx)
	return err
}

type Prepared struct {
	host       framehost.Host
	scope      framehost.ScopeGrant
	plan       framehost.BindingPlan
	converter  framehost.Selection[document.ConverterMetadata, document.Converter]
	validators framehost.Selection[document.ValidatorMetadata, document.Validator]
	output     string
}

// Prepare applies consumer policy to one snapshot of both Points. Acquisition
// later validates that same complete read set; it does not select again.
func (w *Workbench) Prepare(ctx context.Context) (*Prepared, error) {
	snapshot, err := w.Host.Snapshot(ctx, w.Contracts.Converters.Ref(), w.Contracts.Validators.Ref())
	if err != nil {
		return nil, err
	}
	converter, err := framehost.SelectOne(snapshot, w.Contracts.Converters, "authoring.text-priority/1", selectTextConverter)
	if err != nil {
		return nil, err
	}
	validators, err := framehost.SelectMany(snapshot, w.Contracts.Validators, "authoring.validator-order/1", orderValidators)
	if err != nil {
		return nil, err
	}
	plan, err := framehost.Capture(snapshot, converter.Ref(), validators.Ref())
	if err != nil {
		return nil, err
	}
	return &Prepared{host: w.Host, scope: w.scope, plan: plan, converter: converter, validators: validators, output: w.output}, nil
}

func selectTextConverter(values []framehost.Candidate[document.ConverterMetadata, document.Converter]) (framehost.Candidate[document.ConverterMetadata, document.Converter], error) {
	type converterCandidate struct {
		candidate framehost.Candidate[document.ConverterMetadata, document.Converter]
		metadata  document.ConverterMetadata
	}
	converters := make([]converterCandidate, 0, len(values))
	for _, candidate := range values {
		metadata, err := candidate.Metadata()
		if err != nil {
			return framehost.Candidate[document.ConverterMetadata, document.Converter]{}, err
		}
		if metadata.Format == textFormat {
			converters = append(converters, converterCandidate{candidate, metadata})
		}
	}
	if len(converters) == 0 {
		return framehost.Candidate[document.ConverterMetadata, document.Converter]{}, errors.New("authoring: no text converter is available")
	}
	selected := slices.MinFunc(converters, func(a, b converterCandidate) int {
		return cmp.Or(cmp.Compare(b.metadata.Priority, a.metadata.Priority), cmp.Compare(a.candidate.ID(), b.candidate.ID()))
	})
	return selected.candidate, nil
}

func orderValidators(values []framehost.Candidate[document.ValidatorMetadata, document.Validator]) ([]framehost.Candidate[document.ValidatorMetadata, document.Validator], error) {
	type validatorCandidate struct {
		candidate framehost.Candidate[document.ValidatorMetadata, document.Validator]
		metadata  document.ValidatorMetadata
	}
	validators := make([]validatorCandidate, len(values))
	for index, candidate := range values {
		metadata, err := candidate.Metadata()
		if err != nil {
			return nil, err
		}
		validators[index] = validatorCandidate{candidate, metadata}
	}
	slices.SortFunc(validators, func(a, b validatorCandidate) int {
		return cmp.Or(cmp.Compare(a.metadata.Order, b.metadata.Order), cmp.Compare(a.metadata.Name, b.metadata.Name), cmp.Compare(a.candidate.ID(), b.candidate.ID()))
	})
	for index, validator := range validators {
		values[index] = validator.candidate
	}
	return values, nil
}

type Report struct {
	Converted   document.Converted
	Diagnostics []document.Diagnostic
}

// View is an acquired resource result. Its real temporary file and all selected
// providers remain in the original Use until Close or caller-lifetime closure.
type View struct {
	resource *viewResource
}

// Cleanup retains the original resource even if a caller replaces its View.
type viewResource struct {
	use    framehost.Use
	file   *os.File
	size   int64
	report Report
}

func (p *Prepared) Acquire(ctx context.Context, owner frame.Lifetime, reference document.DocumentRef) (view *View, err error) {
	use, err := p.host.Acquire(ctx, p.plan, framehost.UseConfig{Owner: owner, Scope: p.scope})
	if err != nil {
		return nil, err
	}
	defer func() {
		if view == nil {
			_, closeErr := use.Close(context.WithoutCancel(ctx))
			err = errors.Join(err, closeErr)
		}
	}()
	var report Report
	if err := framehost.Borrow(ctx, use, p.converter, func(ctx context.Context, values []document.Converter) error {
		var err error
		report.Converted, err = values[0](ctx, reference)
		return err
	}); err != nil {
		return nil, err
	}
	if err := framehost.Borrow(ctx, use, p.validators, func(ctx context.Context, values []document.Validator) error {
		for _, validate := range values {
			diagnostics, err := validate(ctx, report.Converted)
			if err != nil {
				return err
			}
			report.Diagnostics = append(report.Diagnostics, diagnostics...)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	work, err := use.Enter(ctx)
	if err != nil {
		return nil, err
	}
	defer work.Finish()
	result := &viewResource{use: use, report: report}
	if _, err := work.Cleanup("authored document view", result.releaseFile); err != nil {
		return nil, err
	}
	result.file, err = os.CreateTemp(p.output, "view-*.html")
	if err != nil {
		return nil, err
	}
	written, err := io.WriteString(result.file, report.Converted.HTML)
	if err != nil {
		return nil, err
	}
	if err := frame.ContextError(work.Context()); err != nil {
		return nil, err
	}
	result.size = int64(written)
	return &View{resource: result}, nil
}

func (v *viewResource) releaseFile() error {
	if v.file == nil {
		return nil
	}
	closeErr := v.file.Close()
	removeErr := os.Remove(v.file.Name())
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

func (v *View) Report() Report {
	if v == nil || v.resource == nil {
		return Report{}
	}
	result := v.resource.report
	result.Diagnostics = slices.Clone(result.Diagnostics)
	return result
}

func (v *View) Binding() framehost.BindingSnapshot {
	if v == nil || v.resource == nil {
		return framehost.BindingSnapshot{}
	}
	return v.resource.use.Binding()
}

func (v *View) WithReader(ctx context.Context, read func(context.Context, io.Reader) error) error {
	if v == nil || v.resource == nil {
		return frame.ErrInvalid
	}
	if read == nil {
		return errors.New("authoring: resource reader is required")
	}
	resource := v.resource
	work, err := resource.use.Enter(ctx)
	if err != nil {
		return err
	}
	defer work.Finish()
	return read(work.Context(), io.NewSectionReader(resource.file, 0, resource.size))
}

func (v *View) Close(ctx context.Context) error {
	if v == nil || v.resource == nil {
		return frame.ErrInvalid
	}
	_, err := v.resource.use.Close(ctx)
	return err
}
