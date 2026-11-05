package mcp

// Callback panic causes remain inspectable locally, but their text and embedded
// Refusal values cannot become a backend reply or a confirmed business outcome.
type callbackPanic struct {
	message string
	cause   error
}

func newCallbackPanic(message string, recovered any) *callbackPanic {
	cause, _ := recovered.(error)
	return &callbackPanic{message: message, cause: cause}
}

func (p *callbackPanic) Error() string { return p.message }
func (p *callbackPanic) Unwrap() error { return p.cause }
