package browser

import (
	"errors"
	"testing"

	"github.com/Tangerg/frame/sqlite"
)

func TestReceiptRejectsContradictoryBusinessResults(t *testing.T) {
	realm := Realm{ID: "realm", Epoch: "epoch"}
	grant := UseGrant{OperationID: "operation", ID: "use", Realm: realm}
	publication := Publication{ID: "publication", OperationID: "operation"}
	accepted := PublicationResult{Decision: Accepted, Publication: &publication}
	for _, test := range []struct {
		name    string
		receipt controlReceipt
	}{
		{"acquisition_without_grant", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: acquireOperation, Realm: realm}}},
		{"grant_and_failure", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: acquireOperation, Realm: realm}, Grant: &grant, Failures: []string{closedFailure}}},
		{"grant_and_publication", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: acquireOperation, Realm: realm}, Grant: &grant, Publication: &accepted}},
		{"grant_from_other_operation", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: acquireOperation, Realm: realm}, Grant: &UseGrant{OperationID: "other", ID: "use", Realm: realm}}},
		{"stored_approval_token", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: acquireOperation, Realm: realm}, Grant: &UseGrant{OperationID: "operation", ID: "use", Realm: realm, ApprovalToken: "competing-token"}}},
		{"snapshot_with_grant", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: snapshotOperation, Realm: realm}, Grant: &grant}},
		{"publication_without_result", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: publishOperation, Realm: realm}}},
		{"publication_without_object", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: publishOperation, Realm: realm}, Publication: &PublicationResult{Decision: Accepted}}},
		{"publication_and_refusal", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: publishOperation, Realm: realm}, Publication: &PublicationResult{Decision: Accepted, Publication: &publication, Problem: "refused"}}},
		{"publication_from_other_operation", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: publishOperation, Realm: realm}, Publication: &PublicationResult{Decision: Accepted, Publication: &Publication{ID: "publication", OperationID: "other"}}}},
		{"refusal_with_object", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: publishOperation, Realm: realm}, Publication: &PublicationResult{Decision: Rejected, Publication: &publication, Problem: "refused"}}},
		{"empty_refusal", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: publishOperation, Realm: realm}, Publication: &PublicationResult{Decision: Rejected}}},
		{"unknown_is_not_a_business_result", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: publishOperation, Realm: realm}, Publication: &PublicationResult{Decision: Unknown}}},
		{"unknown_kind", controlReceipt{controlRequestIdentity: controlRequestIdentity{Kind: "unknown", Realm: realm}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation, err := test.receipt.operation("operation", &realmSigner{})
			if operation != nil || !errors.Is(err, sqlite.ErrConflict) {
				t.Fatalf("contradictory receipt became an operation: %+v, %v", operation, err)
			}
		})
	}
}

func TestDeliveryUncertaintyCannotReplaceBusinessResult(t *testing.T) {
	realm := Realm{ID: "realm", Epoch: "epoch"}
	for _, test := range []struct {
		name   string
		kind   operationKind
		settle func(*controlOperation)
		check  func(*testing.T, *controlOperation)
	}{
		{
			"acquired", acquireOperation,
			func(operation *controlOperation) {
				operation.acceptGrant(UseGrant{OperationID: "operation", ID: "use", Realm: realm, Objects: []ObjectRef{{ObjectID: "object"}}})
			},
			func(t *testing.T, operation *controlOperation) {
				grant, err := operation.grantResult()
				if err != nil || grant.ID != "use" || len(grant.Objects) != 1 || grant.Objects[0].ObjectID != "object" {
					t.Fatalf("grant was replaced: %+v, %v", grant, err)
				}
				grant.Objects[0].ObjectID = "changed-observation"
			},
		},
		{
			"published", publishOperation,
			func(operation *controlOperation) {
				operation.acceptPublication(Publication{ID: "publication", OperationID: "operation"})
			},
			func(t *testing.T, operation *controlOperation) {
				result, err := operation.publicationResult()
				if err != nil || result.Decision != Accepted || result.Publication == nil || result.Publication.ID != "publication" {
					t.Fatalf("publication was replaced: %+v, %v", result, err)
				}
				result.Publication.ID = "changed-observation"
			},
		},
		{
			"refused", publishOperation,
			func(operation *controlOperation) { operation.rejectPublication("refused") },
			func(t *testing.T, operation *controlOperation) {
				result, err := operation.publicationResult()
				if err != nil || result.Decision != Rejected || result.Problem != "refused" || result.Publication != nil {
					t.Fatalf("refusal was replaced: %+v, %v", result, err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			operation := &controlOperation{controlRequestIdentity: controlRequestIdentity{Kind: test.kind, Realm: realm}}
			test.settle(operation)
			if err := operation.conclude(captureControlFailure(sqlite.ErrIndeterminate)); !errors.Is(err, sqlite.ErrIndeterminate) {
				t.Fatalf("delivery failure disappeared: %v", err)
			}
			test.check(t, operation)
			receipt := operation.receipt()
			if len(receipt.Failures) != 0 {
				t.Fatalf("delivery failure became business refusal: %v", receipt.Failures)
			}
			restored, err := receipt.operation("operation", &realmSigner{})
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, restored)
			test.check(t, operation)
			if receipt.Grant != nil {
				receipt.Grant.Objects[0].ObjectID = "changed-receipt"
			}
			test.check(t, operation)
		})
	}
}

func TestInterruptedOperationCannotBecomeSuccessful(t *testing.T) {
	for _, kind := range []operationKind{acquireOperation, acquirePlanOperation, publishOperation} {
		t.Run(string(kind), func(t *testing.T) {
			operation := &controlOperation{controlRequestIdentity: controlRequestIdentity{Kind: kind, Realm: Realm{ID: "realm", Epoch: "epoch"}}}
			if err := operation.conclude(nil); !errors.Is(err, sqlite.ErrIndeterminate) {
				t.Fatalf("unfinished operation appeared successful: %v", err)
			}
			restored, err := operation.receipt().operation("operation", &realmSigner{})
			if err != nil {
				t.Fatal(err)
			}
			if kind == publishOperation {
				_, err = restored.publicationResult()
			} else {
				_, err = restored.grantResult()
			}
			if !errors.Is(err, sqlite.ErrIndeterminate) {
				t.Fatalf("unfinished operation restored as success: %v", err)
			}
		})
	}
}
