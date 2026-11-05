package frame_test

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"weak"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

type recoveryBudgetCause struct {
	detail  string
	onError func()
	_       [4096]byte
}

func (e *recoveryBudgetCause) Error() string {
	if e.onError != nil {
		e.onError()
	}
	return e.detail
}

func reportAdditionalRecovery(admission *frame.Admission) []weak.Pointer[recoveryBudgetCause] {
	var reports []weak.Pointer[recoveryBudgetCause]
	for range 64 {
		cause := &recoveryBudgetCause{detail: "later report does not own unresolved resources"}
		admission.RequireRecovery(cause)
		reports = append(reports, weak.Make(cause))
	}
	return reports
}

func TestRecoveryDiagnosticsRetainTheFirstCauseWithoutAccumulatingErrorGraphs(t *testing.T) {
	admission := frame.NewAdmission()
	dependency := frame.Dependency{InstallationID: "original", Release: value.Hash([]byte("original-release"))}
	if err := admission.RegisterUseOwner(&diagnosticUseOwner{id: "consumer.original", values: []frame.Dependency{dependency}}); err != nil {
		t.Fatal(err)
	}
	first := &recoveryBudgetCause{detail: strings.Repeat("original failure ", 1000)}
	first.onError = func() { admission.Scope("diagnostic") }
	admission.RequireRecovery(first)
	before := admission.RecoveryError()
	reports := reportAdditionalRecovery(admission)
	for _, report := range reports {
		awaitCollected(t, report)
	}
	observed := admission.RecoveryError()
	if !errors.Is(observed, frame.ErrRecoveryRequired) || !errors.Is(observed, first) {
		t.Fatalf("bounded diagnostics lost the original recovery cause: %v", observed)
	}
	if original, ok := errors.AsType[*recoveryBudgetCause](observed); !ok || original != first {
		t.Fatal("bounded diagnostics changed the original typed cause")
	}
	if detail := observed.Error(); len(detail) > 2304 || !strings.Contains(detail, "64 additional recovery reports") {
		t.Fatalf("diagnostic projection is unbounded or lost its count: bytes=%d", len(detail))
	}
	if strings.Contains(before.Error(), "additional recovery reports") {
		t.Fatal("later reports mutated an earlier diagnostic snapshot")
	}
	if err := admission.CheckUnused(t.Context(), "original"); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("diagnostic compaction erased an original business dependency: %v", err)
	}
	if err := admission.Recover(t.Context(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := admission.RecoveryError(); err != nil {
		t.Fatalf("successful recovery retained old diagnostics: %v", err)
	}
	next := errors.New("next original failure")
	admission.RequireRecovery(next)
	if err := admission.RecoveryError(); !errors.Is(err, next) || errors.Is(err, first) || strings.Contains(err.Error(), "additional recovery reports") {
		t.Fatalf("new recovery fence inherited the preceding diagnostics: %v", err)
	}
	runtime.KeepAlive(admission)
}

func TestAdditionalRecoveryReportPreventsAnOlderRecoveryFromClearingTheFence(t *testing.T) {
	ctx := testContext(t)
	admission := frame.NewAdmission()
	first := errors.New("original unsettled operation")
	admission.RequireRecovery(first)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- admission.Recover(ctx, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	admission.RequireRecovery(errors.New("another original owner requires settlement"))
	close(release)
	if err := <-finished; !errors.Is(err, frame.ErrRecoveryRequired) || !errors.Is(err, first) {
		t.Fatalf("late recovery cleared a newly reported failure: %v", err)
	}
	called := false
	if err := admission.Under(ctx, func() error { called = true; return nil }); !errors.Is(err, frame.ErrRecoveryRequired) || called {
		t.Fatalf("new admission crossed the retained fence: called=%v, %v", called, err)
	}
	if err := admission.Recover(ctx, func() error { return nil }); err != nil || admission.RecoveryError() != nil {
		t.Fatalf("fresh original-owner recovery could not clear its own fence: %v", err)
	}
	if err := admission.Under(ctx, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("confirmed recovery left admission closed: called=%v, %v", called, err)
	}
}

func TestScopeCapacityRetainsFencedGenerations(t *testing.T) {
	admission, err := frame.NewAdmissionWithLimits(frame.AdmissionLimits{MaxScopes: 1})
	if err != nil {
		t.Fatal(err)
	}
	admission.FenceScope("never-opened")
	if _, exists := admission.Scope("never-opened"); exists {
		t.Fatal("fencing a scope with no authority allocated a tombstone")
	}
	original, err := admission.OpenScope(t.Context(), "original")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admission.OpenScope(t.Context(), "another"); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("scope capacity was not enforced: %v", err)
	}
	if err := admission.ValidateScope(original); err != nil {
		t.Fatalf("capacity refusal revoked the original scope: %v", err)
	}
	admission.FenceScope(original.ID())
	if _, err := admission.OpenScope(t.Context(), "another"); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("a fenced generation was evicted for a new scope: %v", err)
	}
	renewed, err := admission.OpenScope(t.Context(), original.ID())
	if err != nil || renewed.Generation() != original.Generation()+1 {
		t.Fatalf("original scope could not explicitly reopen at capacity: %v, %v", renewed, err)
	}
	if err := admission.ValidateScope(original); !errors.Is(err, frame.ErrFenced) {
		t.Fatalf("reopening revived an old grant: %v", err)
	}
	if _, exists := admission.Scope("another"); exists {
		t.Fatal("refused scope left a partial record")
	}
}

func TestUseOwnerCapacityDoesNotEvictOriginalBusinessReferences(t *testing.T) {
	admission, err := frame.NewAdmissionWithLimits(frame.AdmissionLimits{MaxUseOwners: 1})
	if err != nil {
		t.Fatal(err)
	}
	dependency := frame.Dependency{InstallationID: "original", Release: value.Hash([]byte("release"))}
	original := &diagnosticUseOwner{id: "consumer.original", values: []frame.Dependency{dependency}}
	if err := admission.RegisterUseOwner(original); err != nil {
		t.Fatal(err)
	}
	if err := admission.RegisterUseOwner(&diagnosticUseOwner{id: "consumer.another"}); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("new owner bypassed its capacity: %v", err)
	}
	retained, err := admission.Retained(t.Context(), "original")
	if err != nil || !slices.Equal(retained, []frame.RetainedDependency{{OwnerID: original.ID(), Dependency: dependency}}) {
		t.Fatalf("capacity refusal lost the original business reference: %+v, %v", retained, err)
	}
	if err := admission.CheckUnused(t.Context(), "original"); !errors.Is(err, frame.ErrInUse) {
		t.Fatalf("capacity refusal made an original dependency appear unused: %v", err)
	}
}

func TestInstallationScopeCapacityIsCheckedBeforeTheDurableMutation(t *testing.T) {
	ctx := testContext(t)
	admission, err := frame.NewAdmissionWithLimits(frame.AdmissionLimits{MaxScopes: 1})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "host.db"), Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	host := testHost(t, frame.HostConfig{Admission: admission, Installations: store})
	plugin := installNative(t, host, "original", frame.Unit{
		Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
		Setup:       func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return nil, nil },
	})
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: "another", Plugin: plugin, Enabled: true}); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("installation bypassed scope capacity: %v", err)
	}
	stored, err := store.Installations(ctx)
	if err != nil || len(stored) != 1 || stored[0].ID != "original" {
		t.Fatalf("scope refusal followed a partial durable commit: %+v, %v", stored, err)
	}
	if _, found := host.Installation("another"); found {
		t.Fatal("refused installation was locally published")
	}
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: "disabled", Plugin: plugin}); err != nil {
		t.Fatal(err)
	}
	if err := host.SetEnabled(ctx, "disabled", true); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("enable bypassed scope capacity: %v", err)
	}
	stored, err = store.Installations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range stored {
		if record.ID == "disabled" && record.Enabled {
			t.Fatal("scope refusal durably enabled an installation")
		}
	}
	if err := host.SetEnabled(ctx, "original", false); err != nil {
		t.Fatal(err)
	}
	if err := host.SetEnabled(ctx, "original", true); err != nil {
		t.Fatalf("original installation could not reopen its own scope: %v", err)
	}
	if err := host.Uninstall(ctx, "original"); err != nil {
		t.Fatal(err)
	}
	if err := host.SetEnabled(ctx, "disabled", true); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("uninstall erased a fenced generation: %v", err)
	}
	if _, err := host.Install(ctx, frame.InstallationConfig{ID: "original", Plugin: plugin, Enabled: true}); err != nil {
		t.Fatalf("original identity could not reuse its retained scope record: %v", err)
	}
}

func TestRestorationChecksTheWholeScopeBudgetBeforeProjection(t *testing.T) {
	admission, err := frame.NewAdmissionWithLimits(frame.AdmissionLimits{MaxScopes: 1})
	if err != nil {
		t.Fatal(err)
	}
	host := testHost(t, frame.HostConfig{Admission: admission})
	plugin, err := frame.Define(frame.Definition{
		Revision: value.Hash([]byte("restored")),
		Units: []frame.Unit{{
			Declaration: frame.UnitDeclaration{ID: "main", Profile: frame.NativeGo},
			Setup:       func(context.Context, *frame.Setup) ([]frame.ExportValue, error) { return nil, nil },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	records := []frame.InstallationRecord{
		{ID: "first", Release: plugin.Revision(), Enabled: true, Units: plugin.Units()},
		{ID: "second", Release: plugin.Revision(), Enabled: true, Units: plugin.Units()},
	}
	if err := host.Restore(t.Context(), records, []*frame.Plugin{plugin}); !errors.Is(err, frame.ErrCapacity) {
		t.Fatalf("restoration exceeded its original scope capacity: %v", err)
	}
	for _, record := range records {
		if _, found := host.Installation(record.ID); found {
			t.Fatalf("rejected restoration partially published %s", record.ID)
		}
	}
	if err := host.Restore(t.Context(), records[1:], []*frame.Plugin{plugin}); err != nil {
		t.Fatalf("rejected batch consumed scope capacity: %v", err)
	}
	if err := host.Restore(t.Context(), records[1:], []*frame.Plugin{plugin}); err != nil {
		t.Fatalf("same original restoration consumed another scope: %v", err)
	}
}

func TestNegativeAdmissionBudgetsAreRejected(t *testing.T) {
	for _, limits := range []frame.AdmissionLimits{{MaxScopes: -1}, {MaxUseOwners: -1}} {
		if admission, err := frame.NewAdmissionWithLimits(limits); !errors.Is(err, frame.ErrInvalid) || admission != nil {
			t.Fatalf("invalid admission budget was accepted: %v, %v", admission, err)
		}
	}
}
