package browser_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/browser"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/sqlite"
	"github.com/Tangerg/frame/value"
)

func TestResumedRealmPreservesItsExactBoundedIdentity(t *testing.T) {
	for _, test := range []struct {
		name  string
		realm browser.Realm
	}{
		{"punctuation in id", browser.Realm{ID: "realm.with.dots", Epoch: "epoch"}},
		{"punctuation in epoch", browser.Realm{ID: "realm", Epoch: "epoch.with.dots"}},
		{"bounded components", browser.Realm{ID: strings.Repeat("a", frame.MaxNameBytes), Epoch: strings.Repeat("b", frame.MaxNameBytes)}},
		{"unicode", browser.Realm{ID: "窗口.原始", Epoch: "纪元:一"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
			const resumeToken = "0123456789abcdef0123456789abcdef"
			if err := e.store.RegisterRealm(e.ctx, sqlite.RealmCredential{ID: test.realm.ID, Epoch: test.realm.Epoch, OperationID: "original-realm", ResumeVerifier: value.Hash([]byte(resumeToken))}); err != nil {
				t.Fatal(err)
			}
			credentials, err := e.bridge.ResumeRealm(e.ctx, test.realm, resumeToken)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := e.bridge.Poll(e.ctx, credentials.Token)
			if err != nil || projection.Realm != test.realm {
				t.Fatalf("resumed credential lost its Realm: %+v, %v", projection.Realm, err)
			}
			if err := browser.RealmID(test.realm).Validate(); err != nil {
				t.Fatalf("valid Realm components produced an invalid Frame identity: %v", err)
			}
		})
	}
}

func TestRealmComponentsCannotAliasAnotherRealmsExecutionProof(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), true)
	original := browser.Realm{ID: "realm:epoch", Epoch: "generation"}
	foreign := browser.Realm{ID: "realm", Epoch: "epoch:generation"}
	const resumeToken = "0123456789abcdef0123456789abcdef"
	if err := e.store.RegisterRealm(e.ctx, sqlite.RealmCredential{ID: foreign.ID, Epoch: foreign.Epoch, OperationID: "foreign-realm", ResumeVerifier: value.Hash([]byte(resumeToken))}); err != nil {
		t.Fatal(err)
	}
	credentials, err := e.bridge.ResumeRealm(e.ctx, foreign, resumeToken)
	if err != nil {
		t.Fatal(err)
	}
	source := framehost.UnitRef{InstallationID: "provider", Release: value.Hash([]byte("provider")), UnitID: "view"}
	attempt := sqlite.Attempt{
		ID: "original-execution", Epoch: e.store.Epoch(), Source: source,
		Binding: framehost.BindingSnapshot{Roots: []framehost.ExportRef{}, UnitRoots: []framehost.UnitRef{source}, Units: []framehost.UnitBinding{{Source: source, Configuration: value.Hash(nil)}}},
		Realm:   string(browser.RealmID(original)), Profile: framehost.BrowserJS, Scope: "original-scope", ScopeGeneration: 1, Generation: 1,
	}
	if err := e.store.RecordAttempt(e.ctx, attempt); err != nil {
		t.Fatal(err)
	}
	proof := browser.CloseProof{
		CloseID: "foreign-close", ProofID: "foreign-proof", Authority: credentials.Authority, Realm: foreign,
		Execution: browser.Execution{ID: attempt.ID, Generation: "1"}, ScopeID: string(attempt.Scope), ScopeGeneration: "1",
		ScopeKind: browser.ExecutionScope, Instances: []string{attempt.ID}, Status: frame.ResourceReleased,
	}
	if ack, err := e.bridge.Prove(e.ctx, credentials.Token, proof); !errors.Is(err, framehost.ErrStaleBinding) || ack.Recorded {
		t.Fatalf("another Realm settled the original execution: %+v, %v", ack, err)
	}
	if record, err := e.store.Attempt(e.ctx, attempt.ID); err != nil || record.State != sqlite.AttemptApproved {
		t.Fatalf("foreign proof changed original protection: %+v, %v", record, err)
	}
	if err := e.store.RegisterRealm(e.ctx, sqlite.RealmCredential{ID: original.ID, Epoch: original.Epoch, OperationID: "original-realm", ResumeVerifier: value.Hash([]byte(resumeToken))}); err != nil {
		t.Fatal(err)
	}
	owner, err := e.bridge.ResumeRealm(e.ctx, original, resumeToken)
	if err != nil {
		t.Fatal(err)
	}
	proof.Realm, proof.CloseID, proof.ProofID = original, "original-close", "original-proof"
	if ack, err := e.bridge.Prove(e.ctx, owner.Token, proof); err != nil || !ack.Recorded {
		t.Fatalf("original Realm cannot settle its execution: %+v, %v", ack, err)
	}
}

func TestResumeRealmPreservesCredentialReadFailures(t *testing.T) {
	e := openBridge(t, filepath.Join(t.TempDir(), "state.sqlite"), false)
	t.Cleanup(func() { _ = e.store.Close() })
	credentials, err := e.bridge.OpenRealm(e.ctx, "realm", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for _, realm := range []browser.Realm{credentials.Realm, {ID: "missing", Epoch: "missing"}} {
		if _, err := e.bridge.ResumeRealm(e.ctx, realm, "wrong-token"); !errors.Is(err, framehost.ErrFenced) {
			t.Fatalf("invalid credential classification: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(e.ctx)
	cancel()
	if _, err := e.bridge.ResumeRealm(canceled, credentials.Realm, credentials.ResumeToken); !errors.Is(err, context.Canceled) || errors.Is(err, framehost.ErrFenced) {
		t.Fatalf("cancellation cause was lost: %v", err)
	}
	if err := e.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.bridge.ResumeRealm(e.ctx, credentials.Realm, credentials.ResumeToken); err == nil || errors.Is(err, framehost.ErrFenced) {
		t.Fatalf("storage failure was reported as a credential refusal: %v", err)
	}
}
