package native

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/examples/document"
)

const Installation frame.InstallationID = "authored-document"

type Workbench struct {
	Host      *frame.Host
	Contracts document.Contracts
	scope     frame.ScopeGrant
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
	host, err := frame.NewHost(ctx, frame.HostConfig{})
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
	installed, err := host.Install(ctx, frame.InstallationConfig{ID: Installation, Plugin: plugin, Configuration: configuration, Enabled: true})
	if err != nil {
		return nil, err
	}
	var roots []frame.ExportRef
	for _, unit := range installed.Units {
		for _, export := range unit.Exports {
			roots = append(roots, frame.ExportRef{InstallationID: installed.ID, Release: installed.Release, UnitID: unit.ID, Export: export.Name, Contract: export.Contract})
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
	host       *frame.Host
	scope      frame.ScopeGrant
	plan       *frame.BindingPlan
	converter  frame.Selection[document.ConverterMetadata, document.Converter]
	validators frame.Selection[document.ValidatorMetadata, document.Validator]
	output     string
}

// Prepare applies consumer policy to one snapshot of both Points. Acquisition
// later validates that same complete read set; it does not select again.
func (w *Workbench) Prepare(ctx context.Context) (*Prepared, error) {
	snapshot, err := w.Host.Snapshot(ctx, w.Contracts.Converters.Ref(), w.Contracts.Validators.Ref())
	if err != nil {
		return nil, err
	}
	converter, err := frame.SelectOne(snapshot, w.Contracts.Converters, "authoring.text-priority/1", selectTextConverter)
	if err != nil {
		return nil, err
	}
	validators, err := frame.SelectMany(snapshot, w.Contracts.Validators, "authoring.validator-order/1", orderValidators)
	if err != nil {
		return nil, err
	}
	plan, err := frame.Capture(snapshot, converter.Ref(), validators.Ref())
	if err != nil {
		return nil, err
	}
	return &Prepared{host: w.Host, scope: w.scope, plan: plan, converter: converter, validators: validators, output: w.output}, nil
}

func selectTextConverter(values []frame.Candidate[document.ConverterMetadata, document.Converter]) (frame.Candidate[document.ConverterMetadata, document.Converter], error) {
	type converterCandidate struct {
		candidate frame.Candidate[document.ConverterMetadata, document.Converter]
		metadata  document.ConverterMetadata
	}
	converters := make([]converterCandidate, 0, len(values))
	for _, candidate := range values {
		metadata, err := candidate.Metadata()
		if err != nil {
			return frame.Candidate[document.ConverterMetadata, document.Converter]{}, err
		}
		if metadata.Format == textFormat {
			converters = append(converters, converterCandidate{candidate, metadata})
		}
	}
	if len(converters) == 0 {
		return frame.Candidate[document.ConverterMetadata, document.Converter]{}, errors.New("authoring: no text converter is available")
	}
	selected := slices.MinFunc(converters, func(a, b converterCandidate) int {
		return cmp.Or(cmp.Compare(b.metadata.Priority, a.metadata.Priority), cmp.Compare(a.candidate.ID(), b.candidate.ID()))
	})
	return selected.candidate, nil
}

func orderValidators(values []frame.Candidate[document.ValidatorMetadata, document.Validator]) ([]frame.Candidate[document.ValidatorMetadata, document.Validator], error) {
	type validatorCandidate struct {
		candidate frame.Candidate[document.ValidatorMetadata, document.Validator]
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
	use    *frame.Use
	file   *os.File
	size   int64
	report Report
}

func (p *Prepared) Acquire(ctx context.Context, owner *frame.Lifetime, reference document.DocumentRef) (view *View, err error) {
	use, err := p.host.Acquire(ctx, p.plan, frame.UseConfig{Owner: owner, Scope: p.scope})
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
	if err := frame.Borrow(ctx, use, p.converter, func(ctx context.Context, values []document.Converter) error {
		var err error
		report.Converted, err = values[0](ctx, reference)
		return err
	}); err != nil {
		return nil, err
	}
	if err := frame.Borrow(ctx, use, p.validators, func(ctx context.Context, values []document.Validator) error {
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
	result := &View{use: use, report: report}
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
	if err := contextError(work.Context()); err != nil {
		return nil, err
	}
	result.size = int64(written)
	return result, nil
}

func (v *View) releaseFile() error {
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
	result := v.report
	result.Diagnostics = slices.Clone(result.Diagnostics)
	return result
}

func (v *View) Binding() frame.BindingSnapshot { return v.use.Binding() }

func (v *View) WithReader(ctx context.Context, read func(context.Context, io.Reader) error) error {
	if read == nil {
		return errors.New("authoring: resource reader is required")
	}
	work, err := v.use.Enter(ctx)
	if err != nil {
		return err
	}
	defer work.Finish()
	return read(work.Context(), io.NewSectionReader(v.file, 0, v.size))
}

func (v *View) Close(ctx context.Context) error {
	_, err := v.use.Close(ctx)
	return err
}
