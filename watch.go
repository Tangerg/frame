package frame

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
)

type Change struct {
	Contract ContractRef
	Revision uint64
	Err      error
}

// Copies of a Watch share one subscription; closing any handle releases that
// subscription's capacity and cannot close a later Watch.
type Watch struct{ subscription *catalogWatch }

// The subscription, rather than a caller-owned handle, belongs to the catalog
// and Lifetime. Its host is cleared exactly once, under Host.mu.
type catalogWatch struct {
	host      atomic.Pointer[Host]
	contracts []ContractRef
	changes   chan Change
	stop      func() bool
	resource  *ResourceHandle
}

const watchResourceName = "contract watch"

// Watch atomically registers observation and captures its initial view. A gap
// ends this stream with ErrResyncRequired; it never silently drops a revision.
func (h *Host) Watch(ctx context.Context, points ...ContractRef) (*Snapshot, *Watch, error) {
	var snapshot *Snapshot
	var watch *catalogWatch
	err := h.admission.Under(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.lifetime.isClosing() {
			return ErrClosed
		}
		if len(h.catalog.watches) >= h.limits.MaxWatches {
			return fmt.Errorf("%w: active watches", ErrCapacity)
		}
		var err error
		snapshot, err = h.snapshotLocked("", points)
		if err != nil {
			return err
		}
		watch = &catalogWatch{contracts: slices.Clone(points), changes: make(chan Change, h.limits.WatchBuffer)}
		watch.host.Store(h)
		resource, err := h.lifetime.Own(Resource{
			Name:   watchResourceName,
			Cancel: func(error) { watch.close(ErrClosed) },
			Close: func(context.Context) (ReleaseState, error) {
				watch.close(ErrClosed)
				return ResourceReleased, nil
			},
		})
		if err != nil {
			watch.host.Store(nil)
			return err
		}
		watch.resource = resource
		h.catalog.watches[watch] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { watch.close(nil) })
	h.mu.Lock()
	if watch.host.Load() == nil {
		h.mu.Unlock()
		stop()
	} else {
		watch.stop = stop
		h.mu.Unlock()
	}
	return snapshot, &Watch{subscription: watch}, nil
}

func (w *Watch) Changes() <-chan Change {
	if w == nil || w.subscription == nil {
		return nil
	}
	return w.subscription.changes
}

func (w *Watch) Close() {
	if w != nil && w.subscription != nil {
		w.subscription.close(nil)
	}
}

func (w *catalogWatch) close(err error) {
	host := w.host.Load()
	if host == nil {
		return
	}
	host.mu.Lock()
	w.closeLocked(err)
	host.mu.Unlock()
}

// deliverLocked never blocks the catalog: a full buffer ends the stream.
func (w *catalogWatch) deliverLocked(change Change) {
	select {
	case w.changes <- change:
	default:
		w.closeLocked(ErrResyncRequired)
	}
}

// closeLocked replaces undelivered changes with err, so the terminal reason
// always fits the buffer.
func (w *catalogWatch) closeLocked(err error) {
	host := w.host.Load()
	if host == nil {
		return
	}
	w.host.Store(nil)
	delete(host.catalog.watches, w)
	if w.stop != nil {
		w.stop()
		w.stop = nil
	}
	w.contracts = nil
	if err != nil {
		w.discardBuffered()
		w.changes <- Change{Err: err}
	}
	close(w.changes)
	_ = w.resource.ConfirmReleased(nil)
	w.resource = nil
}

// discardBuffered never blocks: the receiver may drain concurrently.
func (w *catalogWatch) discardBuffered() {
	for {
		select {
		case <-w.changes:
		default:
			return
		}
	}
}
