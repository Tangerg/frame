package process

import (
	"encoding/json/jsontext"
	"reflect"

	"github.com/Tangerg/frame"
	framevalue "github.com/Tangerg/frame/value"
)

// The stdio message variants. Each is identified by exactly the members it
// carries; Go and the Node SDK both admit messages against these shapes.
type (
	requestMessage struct {
		ID     string         `json:"id"`
		Method string         `json:"method"`
		Params jsontext.Value `json:"params"`
	}
	cancelMessage struct {
		Method string         `json:"method"`
		Params jsontext.Value `json:"params"`
	}
	resultMessage struct {
		ID     string         `json:"id"`
		Result jsontext.Value `json:"result"`
	}
	failureMessage struct {
		ID    string  `json:"id"`
		Error Problem `json:"error"`
	}
)

// rawEnvelope keeps every member as sent: a nil member was absent, while an
// explicit null is present.
type rawEnvelope struct {
	ID     jsontext.Value `json:"id"`
	Method jsontext.Value `json:"method"`
	Params jsontext.Value `json:"params"`
	Result jsontext.Value `json:"result"`
	Error  jsontext.Value `json:"error"`
}

func (r rawEnvelope) present() []string {
	var names []string
	value := reflect.ValueOf(r)
	for _, field := range framevalue.Fields(reflect.TypeFor[rawEnvelope]()) {
		if !value.FieldByName(field.GoName).IsNil() {
			names = append(names, field.Name)
		}
	}
	return names
}

// WireShapes is the member set of every stdio message, projected to the Node
// SDK so neither runtime keeps its own copy of the field names.
type WireShapes struct {
	Request     framevalue.Shape `json:"request"`
	Cancel      framevalue.Shape `json:"cancel"`
	Result      framevalue.Shape `json:"result"`
	Failure     framevalue.Shape `json:"failure"`
	Problem     framevalue.Shape `json:"problem"`
	Hello       framevalue.Shape `json:"hello"`
	Preparation framevalue.Shape `json:"preparation"`
	Grant       framevalue.Shape `json:"grant"`
	Source      framevalue.Shape `json:"source"`
	Contract    framevalue.Shape `json:"contract"`
	Call        framevalue.Shape `json:"call"`
	Chunk       framevalue.Shape `json:"chunk"`
	Cancelled   framevalue.Shape `json:"cancelled"`
}

var shapes = Shapes()

func Shapes() WireShapes {
	return WireShapes{
		Request:     framevalue.ShapeOf(reflect.TypeFor[requestMessage]()),
		Cancel:      framevalue.ShapeOf(reflect.TypeFor[cancelMessage]()),
		Result:      framevalue.ShapeOf(reflect.TypeFor[resultMessage]()),
		Failure:     framevalue.ShapeOf(reflect.TypeFor[failureMessage]()),
		Problem:     framevalue.ShapeOf(reflect.TypeFor[Problem]()),
		Hello:       framevalue.ShapeOf(reflect.TypeFor[hello]()),
		Preparation: framevalue.ShapeOf(reflect.TypeFor[preparation]()),
		Grant:       framevalue.ShapeOf(reflect.TypeFor[offered]()),
		Source:      framevalue.ShapeOf(reflect.TypeFor[frame.ExportRef]()),
		Contract:    framevalue.ShapeOf(reflect.TypeFor[frame.ContractRef]()),
		Call:        framevalue.ShapeOf(reflect.TypeFor[callParams]()),
		Chunk:       framevalue.ShapeOf(reflect.TypeFor[chunkParams]()),
		Cancelled:   framevalue.ShapeOf(reflect.TypeFor[cancelParams]()),
	}
}
