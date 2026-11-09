package runner

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Tangerg/frame"
	"github.com/Tangerg/frame/value"
)

func TestControlRequestsHaveClosedOperationParameters(t *testing.T) {
	ref := AttemptRef{AttemptID: "attempt", Epoch: 1, RunnerID: "runner"}
	requests := []struct {
		request  controlRequest
		required []string
	}{
		{identityRequest{}, nil},
		{fenceRequest{Epoch: 1}, []string{"epoch"}},
		{inspectRequest{AttemptID: ref.AttemptID}, []string{"attemptId"}},
		{launchControlRequest{LaunchRequest{
			AttemptRef: ref, Command: "/approved/program", ExecutableDigest: value.Hash([]byte("program")),
			Env: []string{}, Configuration: value.Hash([]byte("config")),
		}}, []string{"attemptId", "epoch", "runnerId", "command", "executableDigest", "env", "configuration"}},
		{closeRequest{ref}, []string{"attemptId", "epoch", "runnerId"}},
		{proveAbsentRequest{ref}, []string{"attemptId", "epoch", "runnerId"}},
		{acknowledgeExitRequest{ExitReceipt{AttemptID: ref.AttemptID, Epoch: ref.Epoch, RunnerID: ref.RunnerID, ProofID: "proof", State: NotCreated}}, []string{"attemptId", "epoch", "runnerId", "proofId", "state"}},
		{attachControlRequest{ref}, []string{"attemptId", "epoch", "runnerId"}},
		{attachStreamRequest{ref}, []string{"attemptId", "epoch", "runnerId"}},
	}
	for _, test := range requests {
		t.Run(string(test.request.method()), func(t *testing.T) {
			encoded, err := encodeRequest(test.request)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := readRequest(bufio.NewReader(bytes.NewReader(encoded)))
			if err != nil || !reflect.DeepEqual(decoded, test.request) {
				t.Fatalf("request changed across wire: %+v %v", decoded, err)
			}
			var envelope controlRequestWire
			if err := json.Unmarshal(encoded, &envelope); err != nil {
				t.Fatal(err)
			}
			for _, field := range append(test.required, "foreign") {
				for _, replacement := range []jsontext.Value{nil, jsontext.Value("null")} {
					if field == "foreign" && replacement == nil {
						continue
					}
					var params map[string]jsontext.Value
					if err := json.Unmarshal(envelope.Params, &params); err != nil {
						t.Fatal(err)
					}
					delete(params, field)
					if replacement != nil {
						params[field] = replacement
					}
					data, err := json.Marshal(params)
					if err != nil {
						t.Fatal(err)
					}
					mutated, err := json.Marshal(controlRequestWire{Operation: envelope.Operation, Params: data})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := readRequest(bufio.NewReader(bytes.NewReader(append(mutated, '\n')))); !errors.Is(err, frame.ErrInvalid) {
						t.Fatalf("admitted missing/null/foreign parameter %q in %s: %v", field, mutated, err)
					}
				}
			}
		})
	}
}

func TestControlRequestEnvelopeRequiresExactlyOneKnownVariant(t *testing.T) {
	for _, input := range []string{
		`null`, `{}`, `{"operation":"identity"}`, `{"operation":"identity","params":null}`,
		`{"operation":null,"params":{}}`, `{"operation":"future","params":{}}`,
		`{"operation":"identity","params":{},"launch":{}}`,
		`{"operation":"identity","params":{"launch":{}}}`,
		`{"operation":"fence","params":{"epoch":1,"receipt":{}}}`,
		`{"operation":"identity","launch":{"command":"ignored"}}`,
		`{"operation":"fence","epoch":1,"receipt":{"proofId":"ignored"}}`,
	} {
		if _, err := readRequest(bufio.NewReader(strings.NewReader(input + "\n"))); !errors.Is(err, frame.ErrInvalid) {
			t.Fatalf("admitted malformed operation %s: %v", input, err)
		}
	}
}

func TestControlResponseEvidenceRequiresEveryNonNullMember(t *testing.T) {
	ref := AttemptRef{AttemptID: "attempt", Epoch: 1, RunnerID: "runner"}
	proof, err := absentProof(ref, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method   controlMethod
		result   controlResult
		required []string
	}{
		{methodIdentity, Identity{ID: "runner", Epoch: 1, Fenced: false}, []string{"id", "epoch", "fenced"}},
		{methodProveAbsent, proof, []string{"attemptId", "epoch", "runnerId", "fenceEpoch", "proofId"}},
		{methodInspect, Observation{AttemptID: "attempt", Epoch: 1, RunnerID: "runner", State: NotCreated, ProofID: "proof"}, []string{"attemptId", "epoch", "runnerId", "state", "proofId"}},
	} {
		data, err := json.Marshal(test.result)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range test.required {
			for _, replacement := range []jsontext.Value{nil, jsontext.Value("null")} {
				var object map[string]jsontext.Value
				if err := json.Unmarshal(data, &object); err != nil {
					t.Fatal(err)
				}
				delete(object, field)
				if replacement != nil {
					object[field] = replacement
				}
				result, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				response, err := json.Marshal(controlResponseWire{Result: result})
				if err != nil {
					t.Fatal(err)
				}
				if admitted, err := decodeResponse(response, test.method); admitted != nil || !errors.Is(err, frame.ErrInvalid) {
					t.Fatalf("invented %s evidence %q in %s: %+v %v", test.method, field, response, admitted, err)
				}
			}
		}
	}
}
