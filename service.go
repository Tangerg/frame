package frame

import (
	"context"
	"errors"

	"github.com/Tangerg/frame/value"
)

// Cancellation and transport loss cannot settle an accepted business effect.
var ErrOutcomeUnknown = errors.New("frame: outcome unknown")

// Service is one already bound export. Only the member names in that export's
// published contract can be invoked; equal member names across exports are independent.
type Service interface {
	Invoke(context.Context, Invocation) (value.JSON, error)
}
type Handler func(context.Context, Invocation) (value.JSON, error)

func (f Handler) Invoke(ctx context.Context, invocation Invocation) (value.JSON, error) {
	return f(ctx, invocation)
}

type Emit func(context.Context, value.JSON) error
type StreamingService interface {
	Service
	Stream(context.Context, Invocation, Emit) (value.JSON, error)
}
