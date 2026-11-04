package browser

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/sqlite"
	framevalue "github.com/Tangerg/frame/value"
)

// A caller-generated resume token must carry enough entropy to be a secret and
// stay small enough to bound what an unauthenticated request can submit.
const (
	minResumeTokenBytes = 32
	maxResumeTokenBytes = 512
)

func RealmID(realm Realm) frame.RealmID { return frame.RealmID(realm.ID + ":" + realm.Epoch) }

// realmOpening joins concurrent attempts of one OpenRealm operation.
type realmOpening struct {
	verifier    framevalue.Digest
	done        chan struct{}
	credentials Credentials
	failure     error
}

// realmSession is a resident Realm. Once cleanupOnly, it starts no new work
// and only drains the executions it already admitted.
type realmSession struct {
	realm       Realm
	application *frame.Lifetime

	cleanupOnly     bool
	projectionBytes int64
}

// Credentials is a trusted in-process composition port. It projects the
// original authenticated session and is deliberately absent from Handler.
// The caller already owns application admission; no second Realm map is needed.
func (b *Bridge) Credentials(realm Realm) (Credentials, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session := b.sessions.get(RealmID(realm))
	if session == nil || session.realm != realm {
		return Credentials{}, frame.ErrUnavailable
	}
	return b.credentials(realm, ""), nil
}

func (b *Bridge) credentials(realm Realm, resumeToken string) Credentials {
	return Credentials{MaxMessageBytes: b.config.MaxMessageBytes, Authority: b.authority, Realm: realm, Token: b.signer.token(realm), ResumeToken: resumeToken}
}

// OpenRealm requires caller-generated credentials before dispatch. Repeating
// the exact operation after a lost response recovers its original Realm, even
// after Host reconstruction; no raw resume credential is persisted.
func (b *Bridge) OpenRealm(ctx context.Context, operationID, resumeToken string) (Credentials, error) {
	if !frame.ValidName(operationID) || len(resumeToken) < minResumeTokenBytes || len(resumeToken) > maxResumeTokenBytes {
		return Credentials{}, frame.ErrInvalid
	}
	verifier := framevalue.Hash([]byte(resumeToken))
	b.mu.Lock()
	if previous := b.openings[operationID]; previous != nil {
		b.mu.Unlock()
		if !sameDigest(previous.verifier, verifier) {
			return Credentials{}, frame.ErrFenced
		}
		select {
		case <-ctx.Done():
			return Credentials{}, cancellation.Err(ctx)
		case <-previous.done:
			return previous.credentials, previous.failure
		}
	}
	if len(b.openings) >= b.config.MaxRealms {
		b.mu.Unlock()
		return Credentials{}, fmt.Errorf("%w: realm openings", frame.ErrCapacity)
	}
	opening := &realmOpening{verifier: verifier, done: make(chan struct{})}
	b.openings[operationID] = opening
	b.mu.Unlock()
	credentials, err := b.openRealm(ctx, operationID, resumeToken)
	b.mu.Lock()
	opening.credentials, opening.failure = credentials, err
	close(opening.done)
	delete(b.openings, operationID)
	b.mu.Unlock()
	return credentials, err
}

func (b *Bridge) openRealm(ctx context.Context, operationID, resumeToken string) (Credentials, error) {
	previous, err := b.config.Store.RealmOperation(ctx, operationID)
	if err == nil {
		return b.ResumeRealm(ctx, Realm{ID: previous.ID, Epoch: previous.Epoch}, resumeToken)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Credentials{}, err
	}
	b.mu.Lock()
	err = b.sessions.reserve()
	b.mu.Unlock()
	if err != nil {
		return Credentials{}, err
	}
	realm := Realm{ID: rand.Text(), Epoch: rand.Text()}
	err = b.config.Store.RegisterRealm(ctx, sqlite.RealmCredential{ID: realm.ID, Epoch: realm.Epoch, OperationID: operationID, ResumeVerifier: framevalue.Hash([]byte(resumeToken))})
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.sessions.cancel()
		return Credentials{}, err
	}
	b.sessions.commit(RealmID(realm), &realmSession{realm: realm, application: frame.NewLifetime(context.Background())})
	return b.credentials(realm, resumeToken), nil
}

func (b *Bridge) ResumeRealm(ctx context.Context, realm Realm, resumeToken string) (Credentials, error) {
	credential, err := b.config.Store.RealmCredential(ctx, realm.ID, realm.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Credentials{}, errors.Join(frame.ErrFenced, err)
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("browser realm credential: %w", err)
	}
	if !sameDigest(framevalue.Hash([]byte(resumeToken)), credential.ResumeVerifier) {
		return Credentials{}, frame.ErrFenced
	}
	return b.credentials(realm, resumeToken), nil
}

// sameDigest compares secret verifiers in constant time.
func sameDigest(a, b framevalue.Digest) bool {
	return subtle.ConstantTimeCompare([]byte(a.String()), []byte(b.String())) == 1
}

// liveSession authenticates a Realm that may still start new work.
func (b *Bridge) liveSession(token string) (*realmSession, error) {
	session, err := b.anySession(token)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if session.cleanupOnly {
		return nil, errRealmSealed
	}
	return session, nil
}

// anySession authenticates a Realm for cleanup as well as normal work. A Realm
// without a resident session, such as one sealed before a Host restart, can
// still deliver the proofs it owes.
func (b *Bridge) anySession(token string) (*realmSession, error) {
	realm, ok := b.signer.authenticate(token)
	if !ok {
		return nil, frame.ErrFenced
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if session := b.sessions.get(RealmID(realm)); session != nil {
		return session, nil
	}
	return &realmSession{realm: realm, cleanupOnly: true}, nil
}

// SealRealm revokes new creation and public admission in this exact Realm.
// Already admitted plans remain protected until their original exit is proven.
// Other windows using the same installation retain their independent scopes.
func (b *Bridge) SealRealm(ctx context.Context, token string) error {
	session, err := b.anySession(token)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.sealSessionLocked(session)
	var entries []*browserExecution
	for _, entry := range b.executions.all() {
		if entry.plan.Realm == session.realm {
			entries = append(entries, entry)
		}
	}
	b.reapRealmLocked(session.realm)
	b.mu.Unlock()
	if session.application != nil {
		session.application.BeginClose(frame.ErrFenced)
	}
	var failures []error
	for _, entry := range entries {
		b.fence(entry)
		if err := b.config.Host.RetireInstance(ctx, entry.attempt.ID); err != nil && !errors.Is(err, frame.ErrUnavailable) {
			failures = append(failures, err)
			continue
		}
		_, err := b.closeExecution(ctx, entry)
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (b *Bridge) sealed(session *realmSession) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return session.cleanupOnly
}

func (b *Bridge) sealSessionLocked(session *realmSession) {
	session.cleanupOnly = true
	for id, snapshot := range b.snapshots {
		if snapshot.realm == session.realm {
			delete(b.snapshots, id)
		}
	}
}

// reapRealmLocked frees a sealed Realm's slot once it owns no execution.
func (b *Bridge) reapRealmLocked(realm Realm) {
	id := RealmID(realm)
	session := b.sessions.get(id)
	if session == nil || !session.cleanupOnly || session.projectionBytes != 0 {
		return
	}
	for _, entry := range b.executions.all() {
		if entry.plan.Realm == realm {
			return
		}
	}
	b.sessions.remove(id)
}
