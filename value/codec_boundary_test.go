package value_test

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"reflect"
	"runtime"
	"testing"

	"github.com/Tangerg/frame/value"
)

type boundaryPhase string

func (boundaryPhase) JSONSchemaEnum() []string { return []string{"draft", "ready"} }

type boundaryRecord struct {
	ID    string        `json:"id"`
	Phase boundaryPhase `json:"phase"`
	Items []string      `json:"items"`
	Pair  [2]bool       `json:"pair"`
	Count int8          `json:"count"`
}

type boundaryCodec struct{ original string }

var errBoundaryCodec = errors.New("test: admitted value reached its codec")

const boundaryRecordJSON = `{"id":"original","phase":"ready","items":[],"pair":[true,false],"count":127}`

func (boundaryCodec) JSONSchemaModel() any { return boundaryRecord{} }
func (boundaryCodec) MarshalJSON() ([]byte, error) {
	return []byte(boundaryRecordJSON), nil
}
func (*boundaryCodec) UnmarshalJSON([]byte) error { return errBoundaryCodec }

func TestDecodeValueUsesThePublishedEnumVocabulary(t *testing.T) {
	descriptor, err := value.SchemaFor[boundaryPhase]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := value.AdmitSchema(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`"invented"`, `""`, `null`, `42`} {
		if err := schema.Validate([]byte(raw)); err == nil {
			t.Fatalf("published vocabulary admitted %s", raw)
		}
		phase := boundaryPhase("ready")
		if err := value.DecodeValue(jsontext.Value(raw), &phase, "phase"); err == nil || phase != "ready" {
			t.Errorf("typed decoding contradicted the original enum for %s: %q, %v", raw, phase, err)
		}
	}
	for _, raw := range []string{`"draft"`, `"ready"`} {
		var phase boundaryPhase
		if err := value.DecodeValue(jsontext.Value(raw), &phase, "phase"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecodeValueAdmitsTheCompleteModelBeforeCallingCodecs(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `[]`,
		`{"id":"changed","phase":"ready","items":[],"pair":[true,false]}`,
		`{"id":"changed","phase":"ready","items":[],"pair":[true,false],"count":128}`,
		`{"id":"changed","phase":"ready","items":[],"pair":[true],"count":127}`,
		`{"id":"changed","phase":"ready","items":null,"pair":[true,false],"count":127}`,
		`{"id":"changed","phase":"invented","items":[],"pair":[true,false],"count":127}`,
		`{"id":42,"phase":"ready","items":[],"pair":[true,false],"count":127}`,
		`{"id":"changed","phase":"ready","items":[],"pair":[true,false],"count":127,"extra":true}`,
	} {
		t.Run(raw, func(t *testing.T) {
			original := boundaryCodec{original: "preserved"}
			err := value.DecodeValue(jsontext.Value(raw), &original, "record")
			if err == nil || errors.Is(err, errBoundaryCodec) || original.original != "preserved" {
				t.Fatalf("invalid wire model reached its codec or changed its owner: %#v, %v", original, err)
			}
		})
	}
	original := boundaryCodec{original: "preserved"}
	if err := value.DecodeValue(jsontext.Value(boundaryRecordJSON), &original, "record"); !errors.Is(err, errBoundaryCodec) || original.original != "preserved" {
		t.Fatalf("admitted codec failure lost its cause or changed the destination: %#v, %v", original, err)
	}
}

func TestDecodeValueChecksSiblingFieldsBeforeCallingACodec(t *testing.T) {
	type request struct {
		First boundaryCodec `json:"first"`
		Last  string        `json:"last"`
	}
	for _, raw := range []string{
		`{"first":` + boundaryRecordJSON + `}`,
		`{"first":` + boundaryRecordJSON + `,"last":42}`,
		`{"first":` + boundaryRecordJSON + `,"last":"kept","extra":true}`,
	} {
		var decoded request
		if err := value.DecodeValue(jsontext.Value(raw), &decoded, "request"); err == nil || errors.Is(err, errBoundaryCodec) {
			t.Errorf("incomplete sibling shape reached its first codec: %s, %v", raw, err)
		}
	}
}

type narrowedPhase struct{ value string }

func (narrowedPhase) JSONSchemaModel() any              { return boundaryPhase("") }
func (narrowedPhase) JSONSchemaEnum() []string          { return []string{"ready", "closed"} }
func (v narrowedPhase) MarshalJSON() ([]byte, error)    { return json.Marshal(v.value) }
func (v *narrowedPhase) UnmarshalJSON(raw []byte) error { return json.Unmarshal(raw, &v.value) }

type wrappedPhase struct{ inner narrowedPhase }

func (wrappedPhase) JSONSchemaModel() any              { return narrowedPhase{} }
func (v wrappedPhase) MarshalJSON() ([]byte, error)    { return json.Marshal(v.inner.value) }
func (v *wrappedPhase) UnmarshalJSON(raw []byte) error { return json.Unmarshal(raw, &v.inner.value) }

func checkComposedEnum[T any](t *testing.T) {
	t.Helper()
	descriptor, err := value.SchemaFor[T]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := value.AdmitSchema(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`"ready"`, `"draft"`, `"closed"`, `"invented"`} {
		var decoded T
		admitted := raw == `"ready"`
		if got := schema.Validate([]byte(raw)) == nil; got != admitted {
			t.Errorf("published model intersection for %s = %v", raw, got)
		}
		if got := value.DecodeValue(jsontext.Value(raw), &decoded, "phase") == nil; got != admitted {
			t.Errorf("decoded model intersection for %s = %v", raw, got)
		}
	}
}

func TestCodecCompositionPreservesEveryOwnedVocabulary(t *testing.T) {
	t.Run("narrowed model", checkComposedEnum[narrowedPhase])
	t.Run("composed model", checkComposedEnum[wrappedPhase])
}

type numericPhase int

func (numericPhase) JSONSchemaEnum() []string { return []string{"ready"} }

type pointerPhase string

func (*pointerPhase) JSONSchemaEnum() []string { return []string{"ready"} }

type panickingPhase string

func (panickingPhase) JSONSchemaEnum() []string { panic(errBoundaryCodec) }

type emptyPhase string

func (emptyPhase) JSONSchemaEnum() []string { return nil }

type duplicatePhase string

func (duplicatePhase) JSONSchemaEnum() []string { return []string{"ready", "ready"} }

type invalidUTF8Phase string

func (invalidUTF8Phase) JSONSchemaEnum() []string { return []string{string([]byte{0xff})} }

func TestEnumConstructionRejectsInvalidDeclarations(t *testing.T) {
	for _, source := range []reflect.Type{reflect.TypeFor[numericPhase](), reflect.TypeFor[pointerPhase](), reflect.TypeFor[emptyPhase](), reflect.TypeFor[duplicatePhase](), reflect.TypeFor[invalidUTF8Phase]()} {
		if err := value.ValidateType(source); err == nil {
			t.Errorf("invalid enum declaration %s was admitted", source)
		}
	}
	t.Run("empty published enum", rejectLossyType[emptyPhase])
	if err := value.ValidateType(reflect.TypeFor[panickingPhase]()); !errors.Is(err, errBoundaryCodec) {
		t.Fatalf("enum declaration panic lost its original cause: %v", err)
	}
}

type exitingDiagnostic struct{}

func (exitingDiagnostic) Error() string {
	runtime.Goexit()
	return ""
}

type failingModel struct{ boundaryCodec }

func (failingModel) JSONSchemaModel() any { panic(exitingDiagnostic{}) }

func TestEncodingModelFailureDoesNotInvokePanicDiagnostics(t *testing.T) {
	done := make(chan struct{})
	var returned bool
	var failure error
	go func() {
		defer close(done)
		_, failure = value.EncodingModel(reflect.TypeFor[failingModel]())
		returned = true
	}()
	<-done
	if !returned || !errors.Is(failure, exitingDiagnostic{}) {
		t.Fatal("formatting the panic diagnostic interrupted declaration failure")
	}
}

func FuzzDecodeValueAgreesWithItsDeclaredModel(f *testing.F) {
	descriptor, err := value.SchemaFor[boundaryCodec]()
	if err != nil {
		f.Fatal(err)
	}
	schema, err := value.AdmitSchema(descriptor)
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{boundaryRecordJSON, `{}`, `null`, `{"phase":"invented"}`, `{"id":"kept","phase":"draft","items":["item"],"pair":[false,true],"count":-128}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		schemaErr := schema.Validate(raw)
		// Transport profile budgets are separate from the declared value shape.
		if errors.Is(schemaErr, value.ErrLimit) {
			return
		}
		var decoded boundaryCodec
		decodeErr := value.DecodeValue(jsontext.Value(raw), &decoded, "record")
		if errors.Is(decodeErr, errBoundaryCodec) != (schemaErr == nil) {
			t.Fatalf("wire model and codec admission disagree for %s: schema=%v, decode=%v", raw, schemaErr, decodeErr)
		}
	})
}
