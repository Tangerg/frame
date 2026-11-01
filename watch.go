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

type Watch struct {
	host      atomic.Pointer[Host]
	contracts []ContractRef
	changes   chan Change
	closed    bool
	stop      func() bool
}

// Watch atomically registers observation and captures its initial view. A gap
// ends this stream with ErrResyncRequired; it never silently drops a revision.
func (h *Host) Watch(ctx context.Context, points ...ContractRef) (*Snapshot, *Watch, error) {
	var snapshot *Snapshot
	var watch *Watch
	err := h.admission.Under(ctx, func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed {
			return ErrClosed
		}
		if len(h.watches) >= h.maxWatches {
			return fmt.Errorf("%w: active watches", ErrCapacity)
		}
		var err error
		snapshot, err = h.snapshotLocked("", points)
		if err != nil {
			return err
		}
		watch = &Watch{contracts: slices.Clone(points), changes: make(chan Change, h.watchCapacity)}
		watch.host.Store(h)
		h.watches[watch] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { watch.Close() })
	h.mu.Lock()
	if watch.closed {
		h.mu.Unlock()
		stop()
	} else {
		watch.stop = stop
		h.mu.Unlock()
	}
	return snapshot, watch, nil
}

func (w *Watch) Changes() <-chan Change { return w.changes }

func (w *Watch) closeLocked(err error) {
	if w.closed {
		return
	}
	w.closed = true
	host := w.host.Load()
	delete(host.watches, w)
	w.host.Store(nil)
	if w.stop != nil {
		w.stop()
		w.stop = nil
	}
	w.contracts = nil
	if err != nil {
		for {
			select {
			case <-w.changes:
				continue
			default:
			}
			break
		}
		w.changes <- Change{Err: err}
	}
	close(w.changes)
}

func (w *Watch) Close() {
	host := w.host.Load()
	if host == nil {
		return
	}
	host.mu.Lock()
	w.closeLocked(nil)
	host.mu.Unlock()
}

func (h *Host) notifyLocked(contract ContractRef, revision uint64) {
	for watch := range h.watches {
		if !slices.Contains(watch.contracts, contract) {
			continue
		}
		select {
		case watch.changes <- Change{Contract: contract, Revision: revision}:
		default:
			watch.closeLocked(ErrResyncRequired)
		}
	}
}
