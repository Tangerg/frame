// Object order and spelling do not change identity; numeric lexemes do. Decoding
// and re-encoding a DTO must not replace the original request evidence.
package jsonvalue

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"

	"github.com/Tangerg/frame/integrity"
)

type Value struct{ value string }

func Parse(data []byte) (Value, error) {
	value := jsontext.Value(append([]byte(nil), data...))
	if err := value.Format(jsontext.ReorderRawObjects(true)); err != nil {
		return Value{}, err
	}
	return Value{value: string(value)}, nil
}

func Encode[T any](value T) (Value, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Value{}, err
	}
	return Parse(data)
}

func (i Value) Validate() error {
	if i.value == "" {
		return errors.New("json value: admitted value is required")
	}
	return nil
}

func (i Value) String() string       { return i.value }
func (i Value) JSON() jsontext.Value { return []byte(i.value) }

func (i Value) Digest() integrity.Digest { return integrity.Hash([]byte(i.value)) }

func (i Value) MarshalJSON() ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	return []byte(i.value), nil
}

func (i *Value) UnmarshalJSON(data []byte) error {
	value, err := Parse(data)
	if err != nil {
		return err
	}
	*i = value
	return nil
}
