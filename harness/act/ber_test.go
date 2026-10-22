package act

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestEncodeLengthUsesTheShortFormBelow128(t *testing.T) {
	for _, n := range []int{0, 1, 127} {
		got := EncodeLength(n)
		if len(got) != 1 {
			t.Fatalf("EncodeLength(%d) wrote %d octets, want 1", n, len(got))
		}
		if int(got[0]) != n {
			t.Errorf("EncodeLength(%d) = %d, want %d", n, got[0], n)
		}
	}
}

func TestEncodeLengthUsesTheLongFormAbove127(t *testing.T) {
	got := EncodeLength(200)
	if len(got) != 2 || got[0] != 0x81 || got[1] != 200 {
		t.Fatalf("EncodeLength(200) = % x, want 81 c8", got)
	}
	got = EncodeLength(256)
	if len(got) != 3 || got[0] != 0x82 || got[1] != 0x01 || got[2] != 0x00 {
		t.Fatalf("EncodeLength(256) = % x, want 82 01 00", got)
	}
	got = EncodeLength(65536)
	if len(got) != 4 || got[0] != 0x83 || got[1] != 0x01 || got[2] != 0x00 || got[3] != 0x00 {
		t.Fatalf("EncodeLength(65536) = % x, want 83 01 00 00", got)
	}
}

func TestEncodeElementRoundTrips(t *testing.T) {
	content := []byte("DC=example,DC=com")
	element := EncodeElement(TagOctetString, content)
	tag, decoded, rest, err := Decode(element)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if tag != TagOctetString {
		t.Errorf("tag = 0x%02x, want 0x%02x", byte(tag), byte(TagOctetString))
	}
	if !bytes.Equal(decoded, content) {
		t.Errorf("content = %q, want %q", decoded, content)
	}
	if len(rest) != 0 {
		t.Errorf("rest = % x, want empty", rest)
	}
}

func TestDecodeLeavesTheRest(t *testing.T) {
	first := EncodeElement(TagInteger, []byte{0x05})
	second := EncodeElement(TagNull, nil)
	tag, content, rest, err := Decode(append(first, second...))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if tag != TagInteger {
		t.Errorf("tag = 0x%02x, want 0x%02x", byte(tag), byte(TagInteger))
	}
	if !bytes.Equal(content, []byte{0x05}) {
		t.Errorf("content = % x, want 05", content)
	}
	if !bytes.Equal(rest, second) {
		t.Errorf("rest = % x, want % x", rest, second)
	}
}

func TestDecodeRefusesTheIndefiniteLength(t *testing.T) {
	_, _, _, err := Decode([]byte{0x30, 0x80, 0x00, 0x00})
	if err == nil {
		t.Fatal("the indefinite length was accepted")
	}
	if !strings.Contains(err.Error(), "indefinite") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
}

func TestDecodeRefusesATruncatedElement(t *testing.T) {
	_, _, _, err := Decode([]byte{0x04, 0x05, 0x01})
	if err == nil {
		t.Fatal("a truncated element was accepted")
	}
	if !errors.Is(err, errTruncated) {
		t.Errorf("the refusal is not the truncation error: %v", err)
	}
}

func TestDecodeRefusesAHostileLength(t *testing.T) {
	// A length that claims four gigabytes must not become an allocation and
	// must not be read as a real length.
	hostile := []byte{0x04, 0x84, 0xff, 0xff, 0xff, 0xff}
	_, _, _, err := Decode(hostile)
	if err == nil {
		t.Fatal("a length of four gigabytes was accepted")
	}
	if !strings.Contains(err.Error(), "bound") {
		t.Errorf("the refusal does not name the bound: %v", err)
	}
}

func TestDecodeRefusesTheHighTagNumberForm(t *testing.T) {
	_, _, _, err := Decode([]byte{0x1f, 0x81, 0x00, 0x00})
	if err == nil {
		t.Fatal("the high-tag-number form was accepted")
	}
	if !strings.Contains(err.Error(), "high-tag-number") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
}

func TestDecodeIntegerReadsSignedValues(t *testing.T) {
	cases := []struct {
		content []byte
		want    int64
	}{
		{[]byte{0x00}, 0},
		{[]byte{0x05}, 5},
		{[]byte{0x00, 0x80}, 128},
		{[]byte{0xff}, -1},
		{[]byte{0x80}, -128},
		{[]byte{0x00, 0xff}, 255},
	}
	for _, tc := range cases {
		got, err := DecodeInteger(tc.content)
		if err != nil {
			t.Fatalf("DecodeInteger(% x): %v", tc.content, err)
		}
		if got != tc.want {
			t.Errorf("DecodeInteger(% x) = %d, want %d", tc.content, got, tc.want)
		}
	}
}

func TestDecodeIntegerRefusesAnEmptyOrOversizedValue(t *testing.T) {
	if _, err := DecodeInteger(nil); err == nil {
		t.Error("an empty integer was accepted")
	}
	if _, err := DecodeInteger(make([]byte, 9)); err == nil {
		t.Error("a nine-byte integer was accepted")
	}
}

func TestDecodeStringReadsTheContent(t *testing.T) {
	if got := DecodeString([]byte("CN=alice")); got != "CN=alice" {
		t.Errorf("DecodeString = %q, want CN=alice", got)
	}
}

// FuzzDecode walks the decoder with whatever the fuzzer hands it.
//
// The decoder is the one place a hostile directory gets to speak first, so it
// must never panic and must never read past the bytes it was given.
func FuzzDecode(f *testing.F) {
	seeds := [][]byte{
		{},
		{0x30},
		{0x30, 0x00},
		{0x30, 0x80, 0x00, 0x00},
		{0x04, 0x03, 'a', 'b', 'c'},
		{0x04, 0x84, 0xff, 0xff, 0xff, 0xff},
		{0x1f, 0x81, 0x00},
		{0x02, 0x01, 0x05},
		{0xa3, 0x06, 0x04, 0x01, 'a', 0x04, 0x01, 'b'},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		tag, content, rest, err := Decode(data)
		if err != nil {
			return
		}
		if len(content)+len(rest) > len(data) {
			t.Fatalf("the decoder returned %d bytes of content and %d of rest for %d bytes of input", len(content), len(rest), len(data))
		}
		if !bytes.Equal(rest, data[len(data)-len(rest):]) {
			t.Fatalf("the rest is not the tail of the input")
		}
		if tag == 0 {
			t.Fatal("a decoded element carried the tag zero")
		}
	})
}
