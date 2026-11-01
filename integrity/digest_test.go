package integrity

import (
	"errors"
	"strings"
	"testing"
)

func TestParseDigestAdmitsOnlyCanonicalSHA256(t *testing.T) {
	valid := strings.Repeat("0123456789abcdef", 4)
	digest, err := ParseDigest(valid)
	if err != nil || digest.String() != valid || digest.Validate() != nil {
		t.Fatalf("ParseDigest(valid) = %q, %v", digest, err)
	}
	for _, text := range []string{"", strings.ToUpper(valid), valid[:63], valid + "0", strings.Repeat("g", 64)} {
		if _, err := ParseDigest(text); !errors.Is(err, ErrInvalidDigest) {
			t.Errorf("ParseDigest(%q) error = %v", text, err)
		}
	}
	if !errors.Is(Digest{}.Validate(), ErrInvalidDigest) {
		t.Fatal("zero digest validated")
	}
}

func TestDigestTextEncodingRoundTripsWithoutRepair(t *testing.T) {
	digest := Hash([]byte("release"))
	text, err := digest.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	var decoded Digest
	if err := decoded.UnmarshalText(text); err != nil || decoded != digest {
		t.Fatalf("round trip = %q, %v", decoded, err)
	}
	if err := decoded.UnmarshalText([]byte(strings.ToUpper(digest.String()))); err == nil {
		t.Fatal("noncanonical digest text decoded")
	}
}

func TestDigestSQLEncodingRoundTripsThroughParsing(t *testing.T) {
	digest := Hash([]byte("release"))
	stored, err := digest.Value()
	if err != nil {
		t.Fatal(err)
	}
	var scanned Digest
	if err := scanned.Scan(stored); err != nil || scanned != digest {
		t.Fatalf("scan = %q, %v", scanned, err)
	}
	if err := scanned.Scan([]byte("not a digest")); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("scan(noncanonical) error = %v", err)
	}
}
