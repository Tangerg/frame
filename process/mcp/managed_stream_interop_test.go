//go:build flameinterop

package mcp

import (
	"context"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	framehost "github.com/Tangerg/frame/host"
	"github.com/Tangerg/frame/process"
	"github.com/Tangerg/frame/value"
)

func TestQimuStreamInterop(t *testing.T) {
	var vectors struct {
		Receipt string `json:"receiptIdentity"`
		Stream  string `json:"streamDelivery"`
	}
	upstream, err := os.ReadFile(os.Getenv("FRAME_QIMU_VECTORS"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(upstream, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.Receipt != receiptFingerprint {
		t.Fatal("upstream receipt identity changed")
	}
	var deliveryDescriptor frame.MethodDescriptor
	if err := value.DecodeValue([]byte(vectors.Stream), &deliveryDescriptor, "upstream chunk delivery"); err != nil {
		t.Fatal(err)
	}
	delivery, err := frame.AdmitMethod(deliveryDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	binary := os.Getenv("FRAME_QIMU_STREAM")
	descriptor, err := exec.CommandContext(t.Context(), binary, "descriptor").Output()
	if err != nil {
		t.Fatal(err)
	}
	contract, err := ParseManagedContract(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.Name() != streamDeliveryName || delivery.Identity().Digest().String() != streamDeliveryFingerprint {
		t.Fatal("upstream delivery identity changed")
	}
	receiver, err := NewCallbackReceiver(CallbackReceiverLimits{MaxCalls: 1, MaxRequestBytes: 1 << 16, MaxResponseBytes: 1 << 16, ReadTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	protocol := ManagedProtocol{Exports: func(framehost.UnitBinding) (map[string]ManagedExport, error) {
		return map[string]ManagedExport{"service": {Name: "records", Contract: contract}}, nil
	}, OpenStream: receiver.Stream}
	checkExecution(t, protocol, contract.Capability(), func(t *testing.T, dir string) process.Program {
		image, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		return process.Program{Command: binary, ExecutableDigest: value.Hash(image), Directory: dir}
	}, nil, func(ctx context.Context, svc frame.Service, _ string) {
		input, err := value.EncodeJSON("two")
		if err != nil {
			t.Fatal(err)
		}
		var chunks []string
		result, err := svc.(frame.StreamingService).Stream(ctx, commandInvocation(t, "original-stream", "records.read", input), func(_ context.Context, chunk value.JSON) error {
			chunks = append(chunks, chunk.String())
			return nil
		})
		if err != nil || result.String() != "true" || !slices.Equal(chunks, []string{"1", "2"}) {
			t.Fatalf("stream %v result=%s err=%v", chunks, result, err)
		}
	})
}
