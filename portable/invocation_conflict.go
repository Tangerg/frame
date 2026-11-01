package portable

import "errors"

var ErrInvocationConflict = errors.New("invocation identity conflicts with the original request")

// InvocationConflictCode is the refusal every method reports for
// ErrInvocationConflict, so no business owner restates it.
const InvocationConflictCode = "command_conflict"
