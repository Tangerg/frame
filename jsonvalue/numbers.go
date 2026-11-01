// Tool-call identifiers, schema bounds, and protocol ids routinely carry
// integers past IEEE-754's exact range, and encoding/json/v2 represents a number
// in an any as a float64, which rounds those silently and refuses magnitudes
// outside float64 entirely.
//
// This is the remedy encoding/json/v2 documents for that case, not a local
// workaround: v2 says to supply an unmarshaler that pre-populates the interface
// with a concrete type that preserves precision. v1's decoder-wide UseNumber is
// the only part of the vocabulary v2 does not export, so it is the only part
// restated here.
package jsonvalue

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
)

// Number carries an exact integer literal as the JSON-schema bound type without
// exposing encoding/json at the call site: a caller that must set a
// jsonschema.Schema bound uses this so that type stays confined to this one owner.
func Number(literal string) jsonv1.Number {
	return jsonv1.Number(literal)
}

// Numbers decodes every JSON number reaching an any as a [jsonv1.Number]
// holding the original literal. It changes nothing else: the unmarshaler
// declines other kinds before reading a token, which is how encoding/json/v2 is
// told to apply its own handling instead.
//
// [jsonv1.Number] is the standard library's exact-number carrier and
// encoding/json/v2 marshals and unmarshals it directly, so a value decoded this
// way re-encodes to the literal it came from.
func Numbers() json.Options {
	return json.WithUnmarshalers(json.UnmarshalFromFunc(decodeNumber))
}

func decodeNumber(decoder *jsontext.Decoder, target *any) error {
	if decoder.PeekKind() != '0' {
		// Read no token first: v2 treats an unsupported call that consumed input
		// as a mutation error rather than a decline.
		return errors.ErrUnsupported
	}
	var number jsonv1.Number
	if err := json.UnmarshalDecode(decoder, &number); err != nil {
		return err
	}
	*target = number
	return nil
}
