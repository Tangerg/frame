package value

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
)

// JSON retains the admitted document. Object order and spelling do not change
// identity, but numeric lexemes do. Its zero value represents absence.
type JSON struct{ document string }

func ParseJSON(data []byte) (JSON, error) {
	value := jsontext.Value(append([]byte(nil), data...))
	if err := value.Format(jsontext.ReorderRawObjects(true)); err != nil {
		return JSON{}, err
	}
	return JSON{document: string(value)}, nil
}

func EncodeJSON[T any](value T) (JSON, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return JSON{}, err
	}
	return ParseJSON(data)
}

func (j JSON) Validate() error {
	if j.document == "" {
		return errors.New("json value: admitted value is required")
	}
	return nil
}

func (j JSON) String() string        { return j.document }
func (j JSON) Bytes() jsontext.Value { return []byte(j.document) }

func (j JSON) Digest() Digest {
	if j.document == "" {
		return Digest{}
	}
	return Hash([]byte(j.document))
}

func (j JSON) MarshalJSON() ([]byte, error) {
	if err := j.Validate(); err != nil {
		return nil, err
	}
	return []byte(j.document), nil
}

func (j *JSON) UnmarshalJSON(data []byte) error {
	value, err := ParseJSON(data)
	if err != nil {
		return err
	}
	*j = value
	return nil
}

func (JSON) JSONSchemaModel() any { return jsonv1.RawMessage{} }
