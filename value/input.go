package value

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
)

// JSON retains the admitted document. Object order and spelling do not change
// identity, but numeric lexemes do. Its zero value represents absence.
type JSON struct{ value string }

func ParseJSON(data []byte) (JSON, error) {
	value := jsontext.Value(append([]byte(nil), data...))
	if err := value.Format(jsontext.ReorderRawObjects(true)); err != nil {
		return JSON{}, err
	}
	return JSON{value: string(value)}, nil
}

func EncodeJSON[T any](value T) (JSON, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return JSON{}, err
	}
	return ParseJSON(data)
}

func (i JSON) Validate() error {
	if i.value == "" {
		return errors.New("json value: admitted value is required")
	}
	return nil
}

func (i JSON) String() string        { return i.value }
func (i JSON) Bytes() jsontext.Value { return []byte(i.value) }

func (i JSON) Digest() Digest {
	if i.value == "" {
		return Digest{}
	}
	return Hash([]byte(i.value))
}

func (i JSON) MarshalJSON() ([]byte, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	return []byte(i.value), nil
}

func (i *JSON) UnmarshalJSON(data []byte) error {
	value, err := ParseJSON(data)
	if err != nil {
		return err
	}
	*i = value
	return nil
}
