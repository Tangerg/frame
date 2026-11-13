package browser

import (
	"testing"

	"github.com/Tangerg/frame"
)

func FuzzRealmCredentialIdentity(f *testing.F) {
	for _, realm := range []Realm{
		{ID: "realm", Epoch: "epoch"},
		{ID: "realm:epoch", Epoch: "generation"},
		{ID: "realm", Epoch: "epoch:generation"},
		{ID: "realm.with.dots", Epoch: "epoch.with.dots"},
		{ID: "窗口.原始", Epoch: "纪元:一"},
		{},
	} {
		f.Add(realm.ID, realm.Epoch)
	}
	key := realmSigner{1}
	f.Fuzz(func(t *testing.T, id, epoch string) {
		realm := Realm{ID: id, Epoch: epoch}
		valid := frame.ValidName(id) && frame.ValidName(epoch)
		decoded, admitted := key.authenticate(key.token(realm))
		if admitted != valid {
			t.Fatalf("credential admission changed the name contract: valid=%t, admitted=%t", valid, admitted)
		}
		if !admitted {
			return
		}
		if decoded != realm {
			t.Fatal("credential changed the original Realm")
		}
		if err := RealmID(realm).Validate(); err != nil {
			t.Fatalf("valid Realm cannot enter Frame affinity: %v", err)
		}
	})
}
