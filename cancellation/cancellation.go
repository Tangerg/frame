// Package cancellation preserves lifetime cancellation and concurrent I/O failures.
package cancellation

import (
	"context"
	"errors"
	"io"
)

// Err retains both the standard cancellation category and the owner's cause.
func Err(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, err) {
		return cause
	}
	return errors.Join(err, cause)
}

// Reader checks cancellation before and after each read; it cannot interrupt
// a read blocked inside r. Errors retain the standard cancellation category,
// its custom cause, and any concurrent I/O failure.
func Reader(ctx context.Context, r io.Reader) io.Reader {
	return reader{ctx: ctx, reader: r}
}

type reader struct {
	ctx    context.Context
	reader io.Reader
}

func (c reader) Read(buffer []byte) (int, error) {
	if err := Err(c.ctx); err != nil {
		return 0, err
	}
	read, err := c.reader.Read(buffer)
	if cancelErr := Err(c.ctx); cancelErr != nil {
		err = errors.Join(err, cancelErr)
	}
	return read, err
}
