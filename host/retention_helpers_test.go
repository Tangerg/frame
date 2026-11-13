package host_test

import (
	"runtime"
	"testing"
	"weak"
)

type retainedPayload [1 << 20]byte

func awaitCollected[T any](t *testing.T, value weak.Pointer[T]) {
	t.Helper()
	for range 100 {
		runtime.GC()
		if value.Value() == nil {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("terminal handle retained its former resource owner")
}
