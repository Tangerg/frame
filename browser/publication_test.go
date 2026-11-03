package browser_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
)

func TestReadyValidationFailureRetainsItsCauseAndOriginalReport(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan, activated := e.beginActivation("publisher")
	report := e.readyReport(plan)
	cancelled, cancel := context.WithCancel(e.ctx)
	cancel()
	result, err := e.bridge.Ready(cancelled, e.credentials.Token, report)
	if !errors.Is(err, context.Canceled) || result.Decision == browser.Rejected {
		t.Fatalf("cancelled validation became a terminal Ready decision: %+v %v", result, err)
	}
	e.host.Admission().RequireRecovery(errors.New("interrupted storage acknowledgement"))
	result, err = e.bridge.Ready(e.ctx, e.credentials.Token, report)
	if recoverErr := e.host.Admission().Recover(e.ctx, func() error { return nil }); recoverErr != nil {
		t.Fatal(recoverErr)
	}
	if !errors.Is(err, frame.ErrRecoveryRequired) || result.Decision == browser.Rejected {
		t.Fatalf("temporary recovery failure became a terminal Ready decision: %+v %v", result, err)
	}
	if result, err = e.bridge.Ready(e.ctx, e.credentials.Token, report); err != nil || result.Decision != browser.Accepted {
		t.Fatalf("original Ready report did not recover: %+v %v", result, err)
	}
	if err := <-activated; err != nil {
		t.Fatal(err)
	}
}

func TestPublicationRefusalRemainsObservableAfterAdmissionRecovers(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("publisher")
	request := browser.PublicationRequest{
		OperationID: "dynamic", Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution,
		Object: browser.ObjectRef{ObjectID: "dynamic-object", Export: browser.LocalExport{
			InstanceID: plan.Units[0].Instance.ID, Name: "dynamic", Contract: e.first.Ref(),
		}},
		MetadataJSON: `{"label":"dynamic"}`,
	}
	e.host.Admission().RequireRecovery(errors.New("interrupted storage acknowledgement"))
	first, err := e.bridge.Publish(e.ctx, e.credentials.Token, request)
	if err != nil || first.Decision != browser.Rejected {
		t.Fatalf("known non-publication became uncertain: %+v %v", first, err)
	}
	if err := e.host.Admission().Recover(e.ctx, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	observed, err := e.bridge.Publication(e.ctx, e.credentials.Token, request.OperationID)
	if err != nil || observed.Decision != browser.Rejected {
		t.Fatalf("healthy observation lost the refusal: %+v %v", observed, err)
	}
	retried, err := e.bridge.Publish(e.ctx, e.credentials.Token, request)
	if err != nil || retried.Decision != browser.Rejected {
		t.Fatalf("original identity did not preserve its refusal: %+v %v", retried, err)
	}
	if _, err := e.host.Publication(plan.Units[0].Instance.ID, request.OperationID); !errors.Is(err, frame.ErrUnavailable) {
		t.Fatalf("refused publication changed the catalog: %v", err)
	}
	request.OperationID = "new-intent"
	if result, err := e.bridge.Publish(e.ctx, e.credentials.Token, request); err != nil || result.Decision != browser.Accepted {
		t.Fatalf("new publication after recovery: %+v %v", result, err)
	}
}

func TestWithdrawalRetriesTheOriginalPublicationAfterCancellation(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "store.db"), true)
	plan := e.start("publisher")
	request := browser.PublicationRequest{
		OperationID: "dynamic", Authority: plan.Authority, Realm: plan.Realm, Execution: plan.Execution,
		Object: browser.ObjectRef{ObjectID: "dynamic-object", Export: browser.LocalExport{
			InstanceID: plan.Units[0].Instance.ID, Name: "dynamic", Contract: e.first.Ref(),
		}},
		MetadataJSON: `{"label":"dynamic"}`,
	}
	published, err := e.bridge.Publish(e.ctx, e.credentials.Token, request)
	if err != nil || published.Decision != browser.Accepted {
		t.Fatalf("publish: %+v %v", published, err)
	}
	withdrawal := browser.Withdrawal{Authority: plan.Authority, PublicationID: published.Publication.ID}
	cancelled, cancel := context.WithCancel(e.ctx)
	cancel()
	if result, err := e.bridge.Withdraw(cancelled, e.credentials.Token, withdrawal); err != nil || result.Decision != browser.Rejected {
		t.Fatalf("cancelled withdrawal became uncertain: %+v %v", result, err)
	}
	if _, err := e.host.Publication(plan.Units[0].Instance.ID, request.OperationID); err != nil {
		t.Fatalf("cancelled withdrawal removed the publication: %v", err)
	}
	for range 2 {
		if result, err := e.bridge.Withdraw(e.ctx, e.credentials.Token, withdrawal); err != nil || result.Decision != browser.Accepted {
			t.Fatalf("original publication did not converge after cancellation: %+v %v", result, err)
		}
	}
	if _, err := e.host.Publication(plan.Units[0].Instance.ID, request.OperationID); !errors.Is(err, frame.ErrUnavailable) {
		t.Fatalf("acknowledged withdrawal left the publication visible: %v", err)
	}
}
