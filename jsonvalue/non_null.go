package jsonvalue

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"io"
)

// UnmarshalNonNull decodes closed typed material. Null is refused before typed
// decoding can turn it into an absent field or a zero-valued decision.
func UnmarshalNonNull(raw []byte, target any) error {
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := decoder.ReadToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if token.Kind() == 'n' {
			return errors.New("json: null is not a typed value")
		}
	}
	return json.Unmarshal(raw, target, json.RejectUnknownMembers(true))
}
