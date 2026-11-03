package value_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Tangerg/frame/portable"
	"github.com/Tangerg/frame/value"
)

type restrictedMapKey string

func (key restrictedMapKey) MarshalText() ([]byte, error) { return []byte(key), nil }
func (key *restrictedMapKey) UnmarshalText(raw []byte) error {
	if string(raw) != "allowed" {
		return errors.New("key is outside the domain")
	}
	*key = restrictedMapKey(raw)
	return nil
}
func (restrictedMapKey) JSONSchemaModel() any { return mapKeyModel("") }

type mapKeyModel string

func (mapKeyModel) JSONSchemaEnum() []string { return []string{"allowed"} }

func TestMapKeysCannotAcquireAnUnprojectedCodec(t *testing.T) {
	for _, sample := range []struct {
		typeOf reflect.Type
		schema func() ([]byte, error)
	}{
		{reflect.TypeFor[map[restrictedMapKey]string](), portable.SchemaFor[map[restrictedMapKey]string]},
		{reflect.TypeFor[map[encoderOnly]string](), portable.SchemaFor[map[encoderOnly]string]},
		{reflect.TypeFor[map[decoderOnly]string](), portable.SchemaFor[map[decoderOnly]string]},
	} {
		t.Run(sample.typeOf.String(), func(t *testing.T) {
			if err := value.ValidateType(sample.typeOf); err == nil {
				t.Fatal("map keys acquired an unprojected codec")
			}
			if _, err := sample.schema(); err == nil {
				t.Fatal("map schema omitted its key codec's constraints")
			}
		})
	}
}

type plainMapKey string

func TestStringMapKeysRetainTheirNames(t *testing.T) {
	checkMapKeyNames[string](t)
	checkMapKeyNames[plainMapKey](t)
}

func checkMapKeyNames[K ~string](t *testing.T) {
	t.Helper()
	rawSchema, err := portable.SchemaFor[map[K]string]()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := portable.AdmitSchema(rawSchema)
	if err != nil {
		t.Fatal(err)
	}
	const raw = `{"arbitrary/name":"value","世界":"字"}`
	if err := schema.Validate([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	var decoded map[K]string
	if err := value.DecodeValue([]byte(raw), &decoded, "map"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, map[K]string{"arbitrary/name": "value", "世界": "字"}) {
		t.Fatalf("map names changed: %v", decoded)
	}
}
