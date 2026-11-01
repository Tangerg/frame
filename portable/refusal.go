package portable

// Refusal is a known business outcome, independent of transport failure.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *Refusal) Error() string { return r.Message }

// Is lets a decoded refusal keep the identity of the conflict it reports.
func (r *Refusal) Is(target error) bool {
	return target == ErrInvocationConflict && r.Code == InvocationConflictCode
}
