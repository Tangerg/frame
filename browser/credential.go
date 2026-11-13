package browser

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"

	"github.com/Tangerg/frame"
)

const (
	realmCredentialPurpose = "realm:"
	useApprovalPurpose     = "use:"
)

// A sealed Realm can authenticate proof retries after its live session is reclaimed.
// The key belongs to this Bridge incarnation; reconstruction requires ResumeRealm.
type realmSigner [sha256.Size]byte

func newRealmSigner() (key realmSigner) { rand.Read(key[:]); return key }

func (key *realmSigner) token(r Realm) string {
	identity := r.encodedIdentity()
	return identity + realmIdentitySeparator + key.signature(realmCredentialPurpose+identity)
}

func (key *realmSigner) approval(id string) string { return key.signature(useApprovalPurpose + id) }

func (key *realmSigner) signature(scope string) string {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(scope))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (key *realmSigner) authenticate(token string) (Realm, bool) {
	componentBytes := base64.RawURLEncoding.EncodedLen(frame.MaxNameBytes)
	signatureBytes := base64.RawURLEncoding.EncodedLen(sha256.Size)
	maximum := componentBytes + len(realmIdentitySeparator) + componentBytes + len(realmIdentitySeparator) + signatureBytes
	if len(token) > maximum {
		return Realm{}, false
	}
	signatureAt := strings.LastIndex(token, realmIdentitySeparator)
	if signatureAt < 0 {
		return Realm{}, false
	}
	realm, ok := parseRealmIdentity(token[:signatureAt])
	if !ok {
		return Realm{}, false
	}
	return realm, hmac.Equal([]byte(token), []byte(key.token(realm)))
}
