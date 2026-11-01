package frame_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/integrity"
	"github.com/Tangerg/frame/jsonvalue"
	"github.com/Tangerg/frame/portable"
)

func TestPortablePointUsesTheOriginalProfileBudgets(t *testing.T) {
	host := testHost(t, frame.HostConfig{})
	descriptor := []byte(`{"protocol":"example","methods":["read"]}`)
	valueRef := frame.ContractRef{Name: "example.point-budget-value", Version: "1", Digest: integrity.Hash(descriptor), Kind: frame.ServiceKind, Mode: frame.PortableMode}
	value, err := frame.BindContract[string](valueRef, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	point, err := frame.NewPortablePoint[jsonvalue.Value]("example.point-budget", "1", []byte(`true`), value)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(value, point); err != nil {
		t.Fatal(err)
	}
	if _, err := host.DecodeMetadata(point.Ref(), []byte(`{"priority":1}`)); err != nil {
		t.Fatalf("bounded metadata was refused: %v", err)
	}
	limits := portable.ProfileLimits()
	tooDeep := []byte(strings.Repeat(`[`, limits.MaxJSONDepth) + `0` + strings.Repeat(`]`, limits.MaxJSONDepth))
	if _, err := host.DecodeMetadata(point.Ref(), tooDeep); !errors.Is(err, portable.ErrLimit) {
		t.Fatalf("Point metadata bypassed the original validation budget: %v", err)
	}
	tooLarge := []byte(`{"description":"` + strings.Repeat("x", limits.MaxDescriptorBytes) + `"}`)
	if _, err := frame.NewPortablePoint[jsonvalue.Value]("example.oversized-point", "1", tooLarge, value); !errors.Is(err, portable.ErrLimit) {
		t.Fatalf("Point constructor bypassed descriptor admission: %v", err)
	}
	oversizedDescriptor := point.Descriptor()
	oversizedDescriptor = append(oversizedDescriptor, bytes.Repeat([]byte(" "), limits.MaxDescriptorBytes+1-len(oversizedDescriptor))...)
	oversizedRef := point.Ref()
	oversizedRef.Digest = integrity.Hash(oversizedDescriptor)
	if _, err := frame.BindPoint[jsonvalue.Value, string](oversizedRef, oversizedDescriptor); !errors.Is(err, portable.ErrLimit) {
		t.Fatalf("typed Point binding bypassed descriptor admission: %v", err)
	}
	if err := host.RegisterDescriptor(oversizedRef, oversizedDescriptor); !errors.Is(err, portable.ErrLimit) {
		t.Fatalf("remote Point registration bypassed descriptor admission: %v", err)
	}
}
