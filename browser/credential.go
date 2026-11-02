package browser

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// A sealed Realm can authenticate proof retries after its live session is reclaimed.
// The key belongs to this Bridge incarnation; reconstruction requires ResumeRealm.
type realmSigner [32]byte

func newRealmSigner() (key realmSigner) { rand.Read(key[:]); return key }

func (key *realmSigner) token(r Realm) string {
	scope := r.ID + "." + r.Epoch
	return scope + "." + key.signature("realm:"+scope)
}

func (key *realmSigner) approval(id string) string { return key.signature("use:" + id) }

func (key *realmSigner) signature(scope string) string {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(scope))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (key *realmSigner) authenticate(token string) (Realm, bool) {
	id, rest, ok := strings.Cut(token, ".")
	if !ok {
		return Realm{}, false
	}
	epoch, _, ok := strings.Cut(rest, ".")
	if !ok {
		return Realm{}, false
	}
	realm := Realm{ID: id, Epoch: epoch}
	return realm, hmac.Equal([]byte(token), []byte(key.token(realm)))
}
