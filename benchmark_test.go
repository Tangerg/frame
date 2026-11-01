package frame_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
)

func benchmarkGraph(b *testing.B, count int) (*frame.Host, frame.BindingSnapshot, frame.ScopeGrant) {
	b.Helper()
	ctx := b.Context()
	host, err := frame.NewHost(ctx, frame.HostConfig{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if outcome, err := host.Close(context.Background()); err != nil || !outcome.Released {
			b.Errorf("close benchmark host: %+v, %v", outcome, err)
		}
	})
	contract, err := frame.NewContract[int]("benchmark.value", "1")
	if err != nil {
		b.Fatal(err)
	}
	if err := host.Register(contract); err != nil {
		b.Fatal(err)
	}
	release := integrity.Hash([]byte(fmt.Sprintf("benchmark-chain-%d", count)))
	ref := func(index int) frame.ExportRef {
		return frame.ExportRef{InstallationID: "benchmark", Release: release, UnitID: frame.UnitID(fmt.Sprintf("unit%d", index)), Export: "value", Contract: contract.Ref()}
	}
	units := make([]frame.Unit, count)
	for index := range count {
		declaration := frame.UnitDeclaration{ID: ref(index).UnitID, Profile: frame.NativeGo, Exports: []frame.ExportDeclaration{{Name: "value", Contract: contract.Ref()}}}
		if index > 0 {
			declaration.Imports = []frame.ImportDeclaration{{Slot: "previous", Contract: contract.Ref(), Providers: []frame.ExportRef{ref(index - 1)}}}
		}
		units[index] = frame.Unit{Declaration: declaration, Setup: func(_ context.Context, setup *frame.Setup) ([]frame.ExportValue, error) {
			value := 0
			if index > 0 {
				var err error
				value, err = frame.Import(setup, "previous", contract)
				if err != nil {
					return nil, err
				}
			}
			return []frame.ExportValue{frame.Provide("value", contract, value+1)}, nil
		}}
	}
	plugin, err := frame.Define(frame.Definition{Revision: release, Units: units})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: "benchmark", Plugin: plugin, Enabled: true}); err != nil {
		b.Fatal(err)
	}
	binding, err := host.Activate(ctx, ref(count-1))
	if err != nil {
		b.Fatal(err)
	}
	scope, err := host.Admission().OpenScope(ctx, "benchmark")
	if err != nil {
		b.Fatal(err)
	}
	return host, binding, scope
}

func BenchmarkNativeValueBorrow(b *testing.B) {
	for _, mode := range []string{"direct", "original-use"} {
		b.Run(mode, func(b *testing.B) {
			host, binding, scope := benchmarkGraph(b, 1)
			owner := frame.NewLifetime(b.Context())
			defer owner.Close(context.Background())
			use, err := acquireExports(host, b.Context(), binding, frame.UseConfig{Owner: owner, Scope: scope})
			if err != nil {
				b.Fatal(err)
			}
			defer use.Close(context.Background())
			consume := func(_ context.Context, value int) error {
				if value != 1 {
					return fmt.Errorf("unexpected value: %d", value)
				}
				return nil
			}
			b.ReportAllocs()
			for b.Loop() {
				if mode == "direct" {
					err = consume(b.Context(), 1)
				} else {
					err = frame.BorrowExport(b.Context(), use, binding.Roots[0], consume)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkBindingAcquireAndClose(b *testing.B) {
	for _, count := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("units-%d", count), func(b *testing.B) {
			host, binding, scope := benchmarkGraph(b, count)
			owner := frame.NewLifetime(b.Context())
			defer owner.Close(context.Background())
			b.ReportAllocs()
			for b.Loop() {
				use, err := acquireExports(host, b.Context(), binding, frame.UseConfig{Owner: owner, Scope: scope})
				if err != nil {
					b.Fatal(err)
				}
				if outcome, err := use.Close(b.Context()); err != nil || !outcome.Released {
					b.Fatalf("close acquired binding: %+v, %v", outcome, err)
				}
			}
		})
	}
}

func BenchmarkUseCancelAndJoin(b *testing.B) {
	host, binding, scope := benchmarkGraph(b, 1)
	owner := frame.NewLifetime(b.Context())
	defer owner.Close(context.Background())
	b.ReportAllocs()
	for b.Loop() {
		use, err := acquireExports(host, b.Context(), binding, frame.UseConfig{Owner: owner, Scope: scope})
		if err != nil {
			b.Fatal(err)
		}
		work, err := use.Lifetime().Enter(b.Context())
		if err != nil {
			b.Fatal(err)
		}
		go func() {
			<-work.Context().Done()
			work.Finish()
		}()
		if outcome, err := use.Close(b.Context()); err != nil || !outcome.Released {
			b.Fatalf("cancel and join original work: %+v, %v", outcome, err)
		}
	}
}
