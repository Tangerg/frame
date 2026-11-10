package sqlite

import (
	"context"
	"slices"

	"github.com/Tangerg/frame"
)

// Terminal labels cannot release protection before their original proof has
// passed the same admission as a direct lookup, including during reconstruction.
type protectionSnapshot struct {
	attempts []AttemptRecord
	uses     []UseRecord
}

func readProtectionSnapshot(ctx context.Context, tx Reader) (protectionSnapshot, error) {
	attempts, err := scanRecords(ctx, tx, attemptRecords+" ORDER BY id", decodeAttempt)
	if err != nil {
		return protectionSnapshot{}, err
	}
	uses, err := scanRecords(ctx, tx, useRecords+" ORDER BY id", decodeUse)
	if err != nil {
		return protectionSnapshot{}, err
	}
	return protectionSnapshot{attempts: attempts, uses: uses}, nil
}

func (s protectionSnapshot) dependencies() []frame.Dependency {
	var dependencies []frame.Dependency
	for _, record := range s.attempts {
		if !record.ExitProven() {
			dependencies = append(dependencies, record.Attempt.Binding.Dependencies()...)
		}
	}
	for _, record := range s.uses {
		if !record.Released {
			dependencies = append(dependencies, record.Approval.Binding.Dependencies()...)
		}
	}
	return frame.CompactDependencies(dependencies)
}

func (s protectionSnapshot) requiresRecovery() bool {
	return slices.ContainsFunc(s.attempts, func(record AttemptRecord) bool { return record.State.pending() }) ||
		slices.ContainsFunc(s.uses, func(record UseRecord) bool { return !record.Released })
}
