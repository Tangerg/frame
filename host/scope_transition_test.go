package host

import (
	"errors"
	"testing"
)

func TestRestoredScopeAdvancesOnlyOriginalAdmittedRevision(t *testing.T) {
	for _, laterFence := range []bool{false, true} {
		name := "original transition"
		if laterFence {
			name = "later fence"
		}
		t.Run(name, func(t *testing.T) {
			admission := NewAdmission()
			key := installationScope("provider")
			original, err := admission.openScope(key)
			if err != nil {
				t.Fatal(err)
			}
			admission.fenceScope(key)
			admitted := admission.captureScope(key)
			if laterFence {
				admission.fenceScope(key)
			}
			restored, err := admission.restoreScope(key, admitted)
			if err != nil {
				t.Fatal(err)
			}
			if err := admission.ValidateScope(original); !errors.Is(err, ErrFenced) {
				t.Fatalf("restoration revived an old grant: %v", err)
			}
			if laterFence {
				if err := admission.ValidateScope(restored); !errors.Is(err, ErrFenced) || restored.Generation() != original.Generation() {
					t.Fatalf("restoration erased a later fence: generation=%d error=%v", restored.Generation(), err)
				}
			} else if err := admission.ValidateScope(restored); err != nil || restored.Generation() != original.Generation()+1 {
				t.Fatalf("committed transition did not receive its new authority: generation=%d error=%v", restored.Generation(), err)
			}
			replayed, err := admission.restoreScope(key, admitted)
			if err != nil || replayed != restored {
				t.Fatalf("replaying original transition advanced authority twice: %+v, %v", replayed, err)
			}
		})
	}
}
