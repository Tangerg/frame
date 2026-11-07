package runner

import (
	"errors"
	"testing"

	"github.com/Tangerg/frame"
)

func TestControlResponsesRejectMissingNullAndForeignResults(t *testing.T) {
	for _, method := range []controlMethod{methodIdentity, methodFence, methodProveAbsent, methodAcknowledgeExit, methodLaunch, methodInspect, methodClose, methodAttachControl, methodAttachStream} {
		for _, data := range []string{`null`, `{}`, `{"result":null}`, `{"error":null}`, `{"error":""}`, `{"error":"unrecognized"}`, `{"result":null,"error":"unknown"}`, `{"result":true,"identity":null}`, `{"result":true,"error":null}`} {
			if response, err := decodeResponse([]byte(data), method); !errors.Is(err, frame.ErrInvalid) || response != nil {
				t.Fatalf("%s admitted %s: %+v %v", method, data, response, err)
			}
		}
	}
	for _, test := range []struct {
		method controlMethod
		data   string
	}{
		{methodFence, `{"result":{"id":"runner","epoch":1,"fenced":false}}`},
		{methodFence, `{"result":false}`},
		{methodFence, `{"result":true,"error":"unknown"}`},
		{methodIdentity, `{"result":true}`},
		{methodIdentity, `{"result":{"id":"runner","epoch":1,"fenced":false},"error":"unknown"}`},
		{methodLaunch, `{"result":{"attemptId":"attempt","epoch":1,"runnerId":"runner","state":"future"}}`},
		{methodLaunch, `{"result":{"attemptId":"attempt","epoch":1,"runnerId":"runner","state":"exited","proofId":"proof"}}`},
		{methodLaunch, `{"result":{"attemptId":"attempt","epoch":1,"runnerId":"runner","state":"not_created","pid":1,"proofId":"proof"}}`},
		{methodLaunch, `{"result":{"attemptId":"attempt","epoch":1,"runnerId":"runner","state":"running","pid":1,"proofId":"proof"}}`},
	} {
		if response, err := decodeResponse([]byte(test.data), test.method); !errors.Is(err, frame.ErrInvalid) || response != nil {
			t.Fatalf("%s admitted %s: %+v %v", test.method, test.data, response, err)
		}
	}
}

func TestControlResponsesAdmitOnlyTheRequestedResultVariant(t *testing.T) {
	for _, method := range []controlMethod{methodFence, methodAcknowledgeExit, methodAttachControl, methodAttachStream} {
		response, err := decodeResponse([]byte(`{"result":true}`), method)
		if _, ok := response.(controlAcknowledgment); err != nil || !ok {
			t.Fatalf("%s acknowledgment: %+v %v", method, response, err)
		}
	}
	response, err := decodeResponse([]byte(`{"result":{"id":"runner","epoch":1,"fenced":false}}`), methodIdentity)
	if _, ok := response.(Identity); err != nil || !ok {
		t.Fatalf("identity: %+v %v", response, err)
	}
	response, err = decodeResponse([]byte(`{"result":{"attemptId":"attempt","epoch":1,"runnerId":"runner","state":"not_created","proofId":"proof"},"error":"capacity"}`), methodLaunch)
	if observation, ok := response.(Observation); !errors.Is(err, frame.ErrCapacity) || !ok || observation.State != NotCreated {
		t.Fatalf("original refusal: %+v %v", response, err)
	}
	response, err = decodeResponse([]byte(`{"result":{"attemptId":"attempt","epoch":1,"runnerId":"runner","state":"exited","pid":1,"exitCode":0,"proofId":"proof"}}`), methodClose)
	if observation, ok := response.(Observation); err != nil || !ok || observation.State != Exited {
		t.Fatalf("original wait: %+v %v", response, err)
	}
	response, err = decodeResponse([]byte(`{"error":"not_found"}`), methodInspect)
	if !errors.Is(err, ErrNotFound) || response != nil {
		t.Fatalf("definitive refusal: %+v %v", response, err)
	}
}
