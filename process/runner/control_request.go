package runner

import (
	"bufio"
	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/Tangerg/frame"
)

type controlRequest interface{ method() controlMethod }

type controlRequestWire struct {
	Operation controlMethod  `json:"operation"`
	Params    jsontext.Value `json:"params"`
}

type identityRequest struct{}
type fenceRequest struct {
	Epoch uint64 `json:"epoch"`
}
type inspectRequest struct {
	AttemptID string `json:"attemptId"`
}
type launchControlRequest struct{ LaunchRequest }
type closeRequest struct{ AttemptRef }
type proveAbsentRequest struct{ AttemptRef }
type acknowledgeExitRequest struct{ ExitReceipt }
type attachControlRequest struct{ AttemptRef }
type attachStreamRequest struct{ AttemptRef }

func (identityRequest) method() controlMethod        { return methodIdentity }
func (fenceRequest) method() controlMethod           { return methodFence }
func (inspectRequest) method() controlMethod         { return methodInspect }
func (launchControlRequest) method() controlMethod   { return methodLaunch }
func (closeRequest) method() controlMethod           { return methodClose }
func (proveAbsentRequest) method() controlMethod     { return methodProveAbsent }
func (acknowledgeExitRequest) method() controlMethod { return methodAcknowledgeExit }
func (attachControlRequest) method() controlMethod   { return methodAttachControl }
func (attachStreamRequest) method() controlMethod    { return methodAttachStream }

func readRequest(reader *bufio.Reader) (controlRequest, error) {
	line, err := readLine(reader)
	if err != nil {
		return nil, err
	}
	wire, err := decodeControlValue[controlRequestWire](line)
	if err != nil {
		return nil, err
	}
	switch wire.Operation {
	case methodIdentity:
		return decodeControlValue[identityRequest](wire.Params)
	case methodFence:
		return decodeControlValue[fenceRequest](wire.Params)
	case methodInspect:
		return decodeControlValue[inspectRequest](wire.Params)
	case methodLaunch:
		return decodeControlValue[launchControlRequest](wire.Params)
	case methodClose:
		return decodeControlValue[closeRequest](wire.Params)
	case methodProveAbsent:
		return decodeControlValue[proveAbsentRequest](wire.Params)
	case methodAcknowledgeExit:
		return decodeControlValue[acknowledgeExitRequest](wire.Params)
	case methodAttachControl:
		return decodeControlValue[attachControlRequest](wire.Params)
	case methodAttachStream:
		return decodeControlValue[attachStreamRequest](wire.Params)
	default:
		return nil, frame.ErrInvalid
	}
}

func encodeRequest(request controlRequest) ([]byte, error) {
	params, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(controlRequestWire{Operation: request.method(), Params: params})
	if err != nil {
		return nil, err
	}
	if len(data)+1 > maxControlBytes {
		return nil, frame.ErrCapacity
	}
	return append(data, '\n'), nil
}
