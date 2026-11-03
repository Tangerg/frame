package mcp

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"

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

func (r *managedResult) UnmarshalJSON(data []byte) error {
	type wire managedResult
	return json.Unmarshal(data, (*wire)(r), json.RejectUnknownMembers(true))
}

func (r *managedResult) unpack() (jsontext.Value, error) {
	if r == nil || (len(r.Value) == 0) == (len(r.Failure) == 0) {
		return nil, errors.New("mcp: managed result requires one value or refusal")
	}
	if len(r.Failure) != 0 {
		var failure Refusal
		if err := framevalue.DecodeValue(r.Failure, &failure, "managed refusal"); err != nil || failure.Code == "" {
			return nil, errors.Join(errors.New("mcp: invalid managed refusal"), err)
		}
		return nil, &failure
	}
	return r.Value, nil
}

func registerManaged(client *sdk.Client) error {
	for _, name := range []string{managedDescribe, managedSetup, managedActivate, managedValidate, managedInvoke} {
		if err := sdk.AddSendingCustomMethod[*managedParams, *managedResult](client, name); err != nil {
			return err
		}
	}
	return nil
}

func managedRaw(ctx context.Context, session *sdk.ClientSession, name string, params *managedParams) (jsontext.Value, error) {
	result, err := sdk.CallCustomMethod[*managedParams, *managedResult](ctx, session, name, params)
	if err != nil {
		return nil, err
	}
	return result.unpack()
}

const jsonNull = "null"

func managedCall(ctx context.Context, session *sdk.ClientSession, name string, params *managedParams, out any) error {
	raw, err := managedRaw(ctx, session, name, params)
	if err != nil {
		return err
	}
	if string(raw) == jsonNull {
		return fmt.Errorf("mcp: %s returned a null protocol result", name)
	}
	return framevalue.DecodeValue(raw, out, name)
}
