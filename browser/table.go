package browser

import (
	"fmt"
	"iter"
	"maps"

	"github.com/Tangerg/frame"
)

// boundedTable is a keyed set whose limit also counts entries still being
// created, so concurrent creations cannot overshoot it. Bridge.mu guards it.
type boundedTable[K comparable, V any] struct {
	name     string
	limit    int
	creating int
	entries  map[K]V
}

func newBoundedTable[K comparable, V any](name string, limit int) boundedTable[K, V] {
	return boundedTable[K, V]{name: name, limit: limit, entries: make(map[K]V)}
}

// reserve claims capacity for one entry; the caller must later commit or
// cancel exactly that reservation.
func (t *boundedTable[K, V]) reserve() error {
	if len(t.entries)+t.creating >= t.limit {
		return fmt.Errorf("%w: %s", frame.ErrCapacity, t.name)
	}
	t.creating++
	return nil
}

func (t *boundedTable[K, V]) cancel() { t.creating-- }

func (t *boundedTable[K, V]) commit(key K, value V) {
	t.creating--
	t.entries[key] = value
}

func (t *boundedTable[K, V]) get(key K) V { return t.entries[key] }

func (t *boundedTable[K, V]) lookup(key K) (V, bool) {
	value, exists := t.entries[key]
	return value, exists
}

func (t *boundedTable[K, V]) remove(key K) { delete(t.entries, key) }

func (t *boundedTable[K, V]) all() iter.Seq2[K, V] { return maps.All(t.entries) }
