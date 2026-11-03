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
package value

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// Opaque numbers keep their lexemes; machine integers consume equivalent
// decimal and exponent spellings without passing through floating-point values.
func Numbers() json.Options {
	return json.WithUnmarshalers(json.JoinUnmarshalers(
		json.UnmarshalFromFunc(decodeNumber),
		json.UnmarshalFromFunc(decodeInteger),
	))
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

func decodeInteger(decoder *jsontext.Decoder, target any) error {
	value := reflect.ValueOf(target).Elem()
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
	default:
		return errors.ErrUnsupported
	}
	if decoder.PeekKind() != '0' || customDecoder(value.Type()) {
		return errors.ErrUnsupported
	}
	raw, err := decoder.ReadValue()
	if err != nil {
		return err
	}
	integer, err := integerDecimal(string(raw))
	if err != nil {
		return err
	}
	if value.CanInt() {
		parsed, err := strconv.ParseInt(integer, 10, value.Type().Bits())
		if err != nil {
			return fmt.Errorf("json integer: value is outside %s: %w", value.Type(), err)
		}
		value.SetInt(parsed)
		return nil
	}
	parsed, err := strconv.ParseUint(integer, 10, value.Type().Bits())
	if err != nil {
		return fmt.Errorf("json integer: value is outside %s: %w", value.Type(), err)
	}
	value.SetUint(parsed)
	return nil
}

// Removing decimal zeroes before applying the exponent keeps exact integer
// conversion bounded by the machine representation, even for enormous exponents.
func integerDecimal(raw string) (string, error) {
	coefficient, exponentText := raw, ""
	if index := strings.IndexAny(raw, "eE"); index >= 0 {
		coefficient, exponentText = raw[:index], raw[index+1:]
	}
	negative := strings.HasPrefix(coefficient, "-")
	coefficient = strings.TrimPrefix(coefficient, "-")
	whole, fraction, _ := strings.Cut(coefficient, ".")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return "0", nil
	}
	significant := strings.TrimRight(digits, "0")
	maximumDigits := len(strconv.FormatUint(math.MaxUint64, 10))
	if len(significant) > maximumDigits {
		return "", errors.New("json integer: value exceeds machine integer precision")
	}
	var exponent int64
	if exponentText != "" {
		var err error
		exponent, err = strconv.ParseInt(exponentText, 10, 64)
		if err != nil {
			return "", errors.New("json integer: exponent cannot produce a machine integer")
		}
	}
	integerExponent := int64(len(fraction) - (len(digits) - len(significant)))
	if exponent < integerExponent || uint64(exponent)-uint64(integerExponent) > uint64(maximumDigits-len(significant)) {
		return "", errors.New("json integer: value is fractional or exceeds machine integer precision")
	}
	integer := significant + strings.Repeat("0", int(uint64(exponent)-uint64(integerExponent)))
	if negative {
		integer = "-" + integer
	}
	return integer, nil
}
