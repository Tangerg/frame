package call

// ConflictCode identifies refusal to reuse an invocation identity for another request.
const ConflictCode = "command_conflict"

// Refusal carries the business owner's reported failure. The value alone does
// not prove that a remote invocation settled; its original receiver owns that fact.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *Refusal) Error() string { return r.Message }

func (r *Refusal) Is(target error) bool {
	return target == ErrConflict && r.Code == ConflictCode
}
