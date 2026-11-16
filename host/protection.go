package host

import (
	"context"
	"fmt"

	"github.com/Tangerg/frame"
)

// UseApproval is the durable record of one surviving execution's Use. Its
// member names are a storage contract: a store may index them.
type UseApproval struct {
	ID              string          `json:"id"`
	ParentID        string          `json:"parentId,omitzero"`
	ExecutionID     string          `json:"executionId,omitzero"`
	Instances       []string        `json:"instances"`
	Realm           RealmID         `json:"realm"`
	Scope           ScopeID         `json:"scope"`
	ScopeGeneration uint64          `json:"scopeGeneration"`
	Epoch           uint64          `json:"epoch"`
	Binding         BindingSnapshot `json:"binding"`
}

func (a UseApproval) Validate() error {
	if err := validateName("use identity", a.ID); err != nil {
		return err
	}
	if a.ParentID != "" {
		if err := validateName("parent use identity", a.ParentID); err != nil {
			return err
		}
		if a.ParentID == a.ID {
			return fmt.Errorf("%w: use cannot be its own parent", frame.ErrInvalid)
		}
	}
	if a.ExecutionID != "" {
		if err := validateName("execution identity", a.ExecutionID); err != nil {
			return err
		}
	}
	if err := a.Realm.Validate(); err != nil {
		return err
	}
	if err := a.Scope.Validate(); err != nil {
		return err
	}
	if a.ScopeGeneration == 0 || a.Epoch == 0 {
		return fmt.Errorf("%w: use authority generation", frame.ErrInvalid)
	}
	instances := make(map[string]struct{}, len(a.Instances))
	for _, id := range a.Instances {
		if err := validateName("instance identity", id); err != nil {
			return err
		}
		if _, duplicate := instances[id]; duplicate {
			return fmt.Errorf("%w: duplicate use instance", frame.ErrInvalid)
		}
		instances[id] = struct{}{}
	}
	return a.Binding.Validate()
}

// ExecutionProtection persists an approved range before a surviving execution
// domain receives it. ApproveUse reports the original commit conclusion; only
// NotCommitted proves that no approval can commit later. A committed approval or
// an indeterminate response requires SettleUse after physical exit. SettleUse must
// either release that original approval or prove it cannot commit. It runs only
// after ApproveUse has returned or its goroutine has exited; neither method may
// leave a detached writer that can later contradict its settlement.
type ExecutionProtection interface {
	ApproveUse(context.Context, UseApproval) (CommitState, error)
	SettleUse(context.Context, string, frame.ReleaseProof) error
}
