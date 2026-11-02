package value

import (
	jsonv1 "encoding/json"
	"errors"
)

// A number owns its original lexeme; neither decoding nor a generated named
// wrapper may silently round it through a machine numeric representation.
type Number struct{ document JSON }

func ParseNumber(data []byte) (Number, error) {
	document, err := ParseJSON(data)
	if err != nil {
		return Number{}, err
	}
	if document.Bytes().Kind() != '0' {
		return Number{}, errors.New("json number: numeric value is required")
	}
	return Number{document: document}, nil
}

func (n Number) String() string               { return n.document.String() }
func (n Number) MarshalJSON() ([]byte, error) { return n.document.MarshalJSON() }
func (n *Number) UnmarshalJSON(data []byte) error {
	parsed, err := ParseNumber(data)
	if err != nil {
		return err
	}
	*n = parsed
	return nil
}
func (Number) JSONSchemaModel() any { return jsonv1.Number("0") }
