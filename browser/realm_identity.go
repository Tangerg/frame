package browser

import (
	"encoding/base64"
	"strings"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/value"
)

const realmIdentitySeparator = "."

// Frame affinity must remain bounded and distinguish the complete tuple even
// when its valid components contain punctuation or fill their own byte limits.
func RealmID(realm Realm) framehost.RealmID {
	return framehost.RealmID(value.Hash([]byte(realm.encodedIdentity())).String())
}

func (r Realm) encodedIdentity() string {
	return base64.RawURLEncoding.EncodeToString([]byte(r.ID)) + realmIdentitySeparator +
		base64.RawURLEncoding.EncodeToString([]byte(r.Epoch))
}

func parseRealmIdentity(encoded string) (Realm, bool) {
	id, epoch, separated := strings.Cut(encoded, realmIdentitySeparator)
	if !separated {
		return Realm{}, false
	}
	idBytes, err := base64.RawURLEncoding.Strict().DecodeString(id)
	if err != nil {
		return Realm{}, false
	}
	epochBytes, err := base64.RawURLEncoding.Strict().DecodeString(epoch)
	if err != nil {
		return Realm{}, false
	}
	realm := Realm{ID: string(idBytes), Epoch: string(epochBytes)}
	return realm, frame.ValidName(realm.ID) && frame.ValidName(realm.Epoch)
}
