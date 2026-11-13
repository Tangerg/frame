package host_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	framehost "github.com/Tangerg/frame/host"
	framevalue "github.com/Tangerg/frame/value"
)

func TestPortablePointUsesTheOriginalProfileBudgets(t *testing.T) {
	host := testHost(t, framehost.Config{})
	descriptor := []byte(`{"protocol":"example","methods":["read"]}`)
	valueRef := framehost.ContractRef{Name: "example.point-budget-value", Version: "1", Digest: framevalue.Hash(descriptor), Kind: framehost.ServiceKind, Mode: framehost.PortableMode}
	value, err := framehost.BindContract[string](valueRef, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	point, err := framehost.NewPortablePoint[framevalue.JSON]("example.point-budget", "1", []byte(`true`), value)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Register(value, point); err != nil {
		t.Fatal(err)
	}
	if _, err := host.PrepareMetadata(point.Ref(), []byte(`{"priority":1}`)); err != nil {
		t.Fatalf("bounded metadata was refused: %v", err)
	}
	limits := framevalue.PortableLimits()
	tooDeep := []byte(strings.Repeat(`[`, limits.MaxJSONDepth) + `0` + strings.Repeat(`]`, limits.MaxJSONDepth))
	if _, err := host.PrepareMetadata(point.Ref(), tooDeep); !errors.Is(err, framevalue.ErrLimit) {
		t.Fatalf("Point metadata bypassed the original validation budget: %v", err)
	}
	tooLarge := []byte(`{"description":"` + strings.Repeat("x", limits.MaxDescriptorBytes) + `"}`)
	if _, err := framehost.NewPortablePoint[framevalue.JSON]("example.oversized-point", "1", tooLarge, value); !errors.Is(err, framevalue.ErrLimit) {
		t.Fatalf("Point constructor bypassed descriptor admission: %v", err)
	}
	oversizedDescriptor := point.Descriptor()
	oversizedDescriptor = append(oversizedDescriptor, bytes.Repeat([]byte(" "), limits.MaxDescriptorBytes+1-len(oversizedDescriptor))...)
	oversizedRef := point.Ref()
	oversizedRef.Digest = framevalue.Hash(oversizedDescriptor)
	if _, err := framehost.BindPoint[framevalue.JSON, string](oversizedRef, oversizedDescriptor); !errors.Is(err, framevalue.ErrLimit) {
		t.Fatalf("typed Point binding bypassed descriptor admission: %v", err)
	}
	if err := host.RegisterDescriptor(oversizedRef, oversizedDescriptor); !errors.Is(err, framevalue.ErrLimit) {
		t.Fatalf("remote Point registration bypassed descriptor admission: %v", err)
	}
}
