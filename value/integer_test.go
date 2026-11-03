package value_test

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"reflect"
	"testing"

	"github.com/Tangerg/frame/value"
)

type signedAlias int64
type unsignedAlias uint64

func TestIntegerDecodingUsesExactNumericValue(t *testing.T) {
	for _, sample := range []struct {
		raw  string
		want any
	}{
		{"1.0", int(1)},
		{"1e0", int8(1)},
		{"-1.2500e2", int16(-125)},
		{"127.000", int8(127)},
		{"-1.28e2", int8(-128)},
		{"2550e-1", uint8(255)},
		{"327670e-1", int16(32767)},
		{"65535.0", uint16(65535)},
		{"2147483647e0", int32(2147483647)},
		{"4294967295.000", uint32(4294967295)},
		{"9.007199254740993e15", signedAlias(9007199254740993)},
		{"-9.223372036854775808e18", int64(-9223372036854775808)},
		{"9.223372036854775807e18", int64(9223372036854775807)},
		{"1.8446744073709551615e19", unsignedAlias(18446744073709551615)},
		{"-0.000e999999999999999999999", uint(0)},
		{"0e-999999999999999999999", int(0)},
	} {
		t.Run(fmt.Sprintf("%s/%T", sample.raw, sample.want), func(t *testing.T) {
			target := reflect.New(reflect.TypeOf(sample.want))
			if err := value.DecodeValue(jsontext.Value(sample.raw), target.Interface(), "value"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(target.Elem().Interface(), sample.want) {
				t.Fatalf("got %v, want %v", target.Elem().Interface(), sample.want)
			}
		})
	}
}

func TestIntegerDecodingRefusesFractionsAndOverflowWithoutPublishing(t *testing.T) {
	for _, sample := range []struct {
		raw string
		dst any
	}{
		{"1.1", new(int)},
		{"1e-1", new(int)},
		{"128e0", new(int8)},
		{"-129.0", new(int8)},
		{"256.0", new(uint8)},
		{"-1e0", new(uint64)},
		{"9223372036854775808.0", new(int64)},
		{"18446744073709551616e0", new(uint64)},
		{"100e9223372036854775807", new(int64)},
		{"1e-9223372036854775808", new(int64)},
		{"1e999999999999999999999", new(int)},
	} {
		t.Run(fmt.Sprintf("%s/%T", sample.raw, sample.dst), func(t *testing.T) {
			before := reflect.ValueOf(sample.dst).Elem().Interface()
			if err := value.DecodeValue(jsontext.Value(sample.raw), sample.dst, "value"); err == nil {
				t.Fatal("unrepresentable integer admitted")
			}
			if !reflect.DeepEqual(reflect.ValueOf(sample.dst).Elem().Interface(), before) {
				t.Fatal("refused integer changed its destination")
			}
		})
	}
}

type customInteger int

func (n *customInteger) UnmarshalJSON(raw []byte) error {
	if string(raw) != "1.0" {
		return fmt.Errorf("unexpected custom input %s", raw)
	}
	*n = 42
	return nil
}

func TestIntegerDecodingPreservesCustomDecoderAuthority(t *testing.T) {
	var result struct {
		Custom customInteger  `json:"custom"`
		List   []signedAlias  `json:"list"`
		Map    map[string]int `json:"map"`
	}
	if err := json.Unmarshal([]byte(`{"custom":1.0,"list":[2e0],"map":{"n":3.0}}`), &result, value.Numbers()); err != nil {
		t.Fatal(err)
	}
	if result.Custom != 42 || result.List[0] != 2 || result.Map["n"] != 3 {
		t.Fatalf("nested or custom decoding changed: %+v", result)
	}
}
