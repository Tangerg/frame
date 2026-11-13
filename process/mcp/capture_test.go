package mcp

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

func TestManagedContractOwnsItsCapabilityProjection(t *testing.T) {
	method, err := frame.NewMethod[string, string](frame.MethodConfig{Name: "read", Kind: frame.Query, Target: "document"})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := value.EncodeJSON(managedDescriptor{managedVersion, "documents.v1", managedSelection, "installation", []frame.MethodDescriptor{method.Descriptor()}})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := ParseManagedContract(descriptor.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	projection := contract.Capability()
	identity := projection.Identity()
	*projection = frame.Capability{}
	if contract.Capability().Identity() != identity {
		t.Fatal("capability projection changed the managed contract's admitted identity")
	}
}

func TestCallbackGrantRetainsItsAdmittedMethod(t *testing.T) {
	receiver, err := NewCallbackReceiver(CallbackReceiverLimits{1, 1024, 1024, time.Second})
	if err != nil {
		t.Fatal(err)
	}
	method := callbackTestMethod(t)
	body := callbackBody(t, method, `"content"`)
	grant, err := receiver.Grant(t.Context(), method, func(_ context.Context, _ string, input value.JSON) (value.JSON, error) {
		return input, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := grant.Revoke(); err != nil {
			t.Error(err)
		}
	})
	replacement, err := frame.NewMethod[int, bool](frame.MethodConfig{Name: method.Name(), Kind: frame.Command})
	if err != nil {
		t.Fatal(err)
	}
	*method = *replacement
	status, reply, err := callbackPost(t.Context(), grant, body)
	if err != nil || status != http.StatusOK || reply != `{"value":"content"}` {
		t.Fatalf("caller replaced a granted callback's schema: status=%d reply=%s error=%v", status, reply, err)
	}
}

func TestManagedCallBrokerCannotReplaceTheAdmittedMethod(t *testing.T) {
	method := callbackTestMethod(t)
	identity := method.Identity()
	input, err := value.EncodeJSON("content")
	if err != nil {
		t.Fatal(err)
	}
	service := ManagedService{lifetime: frame.NewLifetime(t.Context()), openCall: func(_ context.Context, _ *sdk.ClientSession, call ManagedCall) (*ManagedCallback, error) {
		*call.Method = frame.Method{}
		return nil, nil
	}}
	var metadata sdk.ParamsBase
	finish, err := service.grantCallback(t.Context(), method, commandInvocation(t, "original", method.Name(), input), &metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if method.Identity() != identity {
		t.Fatal("callback broker replaced the method used for the original dispatch")
	}
}
