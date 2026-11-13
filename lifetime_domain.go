package frame

import (
	"context"
)

// A domain cannot be recovered from a Lifetime, so a consumer cannot issue
// another Host's internal lifetime without holding that original authority.
type LifetimeDomain struct{ _ byte }

func (d *LifetimeDomain) NewLifetime(parent context.Context) Lifetime {
	if d == nil {
		panic(ErrInvalid)
	}
	l := NewLifetime(parent)
	l.authority = d
	return l
}
func (d *LifetimeDomain) Owns(l Lifetime) bool { return d != nil && l.Valid() && l.authority == d }
func (l *lifetimeRecord) HasDomain() bool      { return l != nil && l.authority != nil }
