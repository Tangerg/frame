package frame

import (
	"context"
	"fmt"
)

type UseApproval struct {
	ID              string
	ParentID        string
	ExecutionID     string
	Instances       []string
	Realm           RealmID
	Scope           ScopeID
	ScopeGeneration uint64
	Epoch           uint64
	Binding         BindingSnapshot
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
			return fmt.Errorf("%w: use cannot be its own parent", ErrInvalid)
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
		return fmt.Errorf("%w: use authority generation", ErrInvalid)
	}
	instances := make(map[string]struct{}, len(a.Instances))
	for _, id := range a.Instances {
		if err := validateName("instance identity", id); err != nil {
			return err
		}
		if _, duplicate := instances[id]; duplicate {
			return fmt.Errorf("%w: duplicate use instance", ErrInvalid)
		}
		instances[id] = struct{}{}
	}
	return a.Binding.Validate()
}

// ExecutionProtection persists an approved range before a surviving execution
// domain receives it. Release observes the same original use, never a new one.
type ExecutionProtection interface {
	ApproveUse(context.Context, UseApproval) error
	ReleaseUse(context.Context, string, ReleaseProof) error
}
