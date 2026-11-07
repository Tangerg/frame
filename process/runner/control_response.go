package runner

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"net"
	"time"

	"github.com/Tangerg/frame"
)

type controlResult interface{ controlResult() }

type controlAcknowledgment struct{}

func (controlAcknowledgment) controlResult() {}
func (Observation) controlResult()           {}
func (Identity) controlResult()              {}
func (AbsenceProof) controlResult()          {}

type controlResponseWire struct {
	Result jsontext.Value `json:"result,omitempty"`
	Error  jsontext.Value `json:"error,omitempty"`
}

func acknowledged(err error) (controlResult, error) {
	if err != nil {
		return nil, err
	}
	return controlAcknowledgment{}, nil
}

func (s *Server) writeAcknowledgment(writer net.Conn, err error) error {
	result, failure := acknowledged(err)
	return s.writeReply(writer, result, failure)
}

func (s *Server) writeReply(writer net.Conn, response controlResult, failure error) error {
	if response == nil && failure == nil {
		return frame.ErrInvalid
	}
	if err := writer.SetWriteDeadline(time.Now().Add(s.config.ControlTimeout)); err != nil {
		return err
	}
	var wire controlResponseWire
	if response != nil {
		var result any = response
		if _, ok := response.(controlAcknowledgment); ok {
			result = true
		}
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		wire.Result = data
	}
	if failure != nil {
		data, err := json.Marshal(errorCode(failure))
		if err != nil {
			return err
		}
		wire.Error = data
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	return writeControl(writer, append(data, '\n'))
}

func decodeResponse(data []byte, method controlMethod) (controlResult, error) {
	var wire *controlResponseWire
	if err := json.Unmarshal(data, &wire, json.RejectUnknownMembers(true)); err != nil {
		return nil, errors.Join(frame.ErrInvalid, err)
	}
	if wire == nil {
		return nil, frame.ErrInvalid
	}
	failure, err := decodeControlError(wire.Error)
	if err != nil {
		return nil, err
	}
	if len(wire.Result) == 0 {
		if failure == nil {
			return nil, frame.ErrInvalid
		}
		return nil, failure
	}
	result, err := decodeControlResult(wire.Result, method)
	if err != nil {
		return nil, errors.Join(frame.ErrInvalid, err)
	}
	// Failed acknowledgments and identity queries cannot also claim success.
	// Attempt observations remain useful beside a failed close or launch.
	if failure != nil {
		if _, ok := result.(Observation); !ok {
			return nil, frame.ErrInvalid
		}
	}
	return result, failure
}

func decodeControlResult(data []byte, method controlMethod) (controlResult, error) {
	switch method {
	case methodFence, methodAcknowledgeExit, methodAttachControl, methodAttachStream:
		result, err := decodeControlValue[bool](data)
		if err != nil || !result {
			return nil, errors.Join(frame.ErrInvalid, err)
		}
		return controlAcknowledgment{}, nil
	case methodIdentity:
		identity, err := decodeControlValue[Identity](data)
		if err != nil || !frame.ValidName(identity.ID) || identity.Epoch == 0 && !identity.Fenced {
			return nil, errors.Join(frame.ErrInvalid, err)
		}
		return identity, nil
	case methodProveAbsent:
		proof, err := decodeControlValue[AbsenceProof](data)
		if err != nil || proof.Validate(AttemptRef{AttemptID: proof.AttemptID, Epoch: proof.Epoch, RunnerID: proof.RunnerID}) != nil {
			return nil, errors.Join(frame.ErrInvalid, err)
		}
		return proof, nil
	case methodLaunch, methodInspect, methodClose:
		observation, err := decodeControlValue[Observation](data)
		if err != nil || observation.validate() != nil {
			return nil, errors.Join(frame.ErrInvalid, err)
		}
		return observation, nil
	default:
		return nil, frame.ErrInvalid
	}
}

func decodeControlValue[T any](data []byte) (T, error) {
	var result *T
	err := json.Unmarshal(data, &result, json.RejectUnknownMembers(true))
	if err != nil || result == nil {
		var zero T
		return zero, errors.Join(frame.ErrInvalid, err)
	}
	return *result, nil
}

func decodeControlError(data []byte) (error, error) {
	if len(data) == 0 {
		return nil, nil
	}
	code, err := decodeControlValue[string](data)
	if err != nil {
		return nil, err
	}
	for _, category := range controlErrors {
		if category.code == code {
			return category.err, nil
		}
	}
	if code == unclassifiedControlError {
		return errors.New("runner: control request failed"), nil
	}
	return nil, frame.ErrInvalid
}
