package mcp

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"

	"github.com/Tangerg/frame/internal/cancellation"
	"github.com/Tangerg/frame/portable"
	framevalue "github.com/Tangerg/frame/value"
	sdk "github.com/Tangerg/go-sdk/mcp"
)

const (
	managedDescribe   = managedNamespace + "operations.describe"
	managedSetup      = managedNamespace + "operations.setup"
	managedActivate   = managedNamespace + "operations.activate"
	managedValidate   = managedNamespace + "operations.validate"
	managedInvoke     = managedNamespace + "operations.invoke"
	managedReceipt    = managedNamespace + "commands.get"
	managedInvocation = managedNamespace + "invocation"
	// Exact commands.get contract in Qimu portable/1's published protocol vectors.
	receiptFingerprint = "b21fbad6020077941d9ae1b7bf8b5e4e31572e83aac566f2f357e745ead39e9a"
)

type managedContribution struct {
	ID         string              `json:"id"`
	Export     string              `json:"export"`
	Requires   []managedDependency `json:"requires"`
	Background *portable.Target    `json:"background,omitzero"`
}

type managedParams struct {
	sdk.ParamsBase
	Protocol      string                 `json:"protocol"`
	Contributions *[]managedContribution `json:"contributions,omitzero"`
	Method        string                 `json:"method,omitzero"`
	Contract      framevalue.Digest      `json:"contract,omitzero"`
	Export        string                 `json:"export,omitzero"`
	Capability    framevalue.Digest      `json:"capability,omitzero"`
	Input         jsontext.Value         `json:"input,omitzero"`
	CommandID     string                 `json:"commandId,omitzero"`
}

type managedResult struct {
	sdk.ResultBase
	Value   jsontext.Value `json:"value,omitzero"`
	Failure jsontext.Value `json:"failure,omitzero"`
}

// A refusal is evidence only when decoded from the original backend reply.
// Local errors keep their causes without acquiring that authority.
type managedReply struct {
	value   jsontext.Value
	refusal *Refusal
}

func (r managedReply) result() (jsontext.Value, error) {
	if r.refusal != nil {
		return nil, r.refusal
	}
	return r.value, nil
}

func (r *managedResult) UnmarshalJSON(data []byte) error {
	type wire managedResult
	return json.Unmarshal(data, (*wire)(r), json.RejectUnknownMembers(true))
}

func (r *managedResult) unpack() (managedReply, error) {
	if r == nil || (len(r.Value) == 0) == (len(r.Failure) == 0) {
		return managedReply{}, errors.New("mcp: managed result requires one value or refusal")
	}
	if len(r.Failure) != 0 {
		var failure Refusal
		if err := framevalue.DecodeValue(r.Failure, &failure, "managed refusal"); err != nil || failure.Code == "" {
			return managedReply{}, errors.Join(errors.New("mcp: invalid managed refusal"), err)
		}
		return managedReply{refusal: &failure}, nil
	}
	return managedReply{value: r.Value}, nil
}

func registerManaged(client *sdk.Client) error {
	for _, name := range []string{managedDescribe, managedSetup, managedActivate, managedValidate, managedInvoke} {
		if err := sdk.AddSendingCustomMethod[*managedParams, *managedResult](client, name); err != nil {
			return err
		}
	}
	return nil
}

func requestManaged(ctx context.Context, session *sdk.ClientSession, name string, params *managedParams) (managedReply, error) {
	result, err := sdk.CallCustomMethod[*managedParams, *managedResult](ctx, session, name, params)
	if err != nil {
		return managedReply{}, errors.Join(err, cancellation.Err(ctx))
	}
	return result.unpack()
}

const jsonNull = "null"

func managedCall(ctx context.Context, session *sdk.ClientSession, name string, params *managedParams, out any) error {
	reply, err := requestManaged(ctx, session, name, params)
	if err != nil {
		return err
	}
	raw, err := reply.result()
	if err != nil {
		return err
	}
	if string(raw) == jsonNull {
		return fmt.Errorf("mcp: %s returned a null protocol result", name)
	}
	return framevalue.DecodeValue(raw, out, name)
}
