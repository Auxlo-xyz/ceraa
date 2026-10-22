// Package act reaches into an Active Directory: the LDAP session that reads
// the directory and the Kerberos exchange that roasts a service ticket.
//
// The two protocols live here together because they answer the same two
// questions about a domain, and an operator who has one usually needs the
// other: who is in the directory, and which of those accounts can be
// roasted. LDAP names the accounts; Kerberos says which of them are worth an
// hour.
//
// The ground rules are the harness's own:
//
//   - The engagement record is the authorization. recon.Gate is the gate for
//     HTTP and takes a URL; LDAP and Kerberos are not HTTP, so the decision
//     is made from the same record, read directly. A host the record does
//     not name is refused, and the refusal says which host.
//   - The egress wall is the wall. netguard still refuses loopback, private
//     and link-local space at connect time unless the operator's engagement
//     names the range, so an internal engagement works only when the
//     operator has opened it on purpose.
//   - Nothing unbounded. Every read, every list and every reply is capped,
//     because a directory that answers with a hostile length must not be
//     able to take the process down with one packet.
//
// The wire formats are implemented here rather than pulled in. LDAP is BER,
// which encoding/asn1 does not speak, so ber.go carries a small codec; and
// a format reader must never add a dependency that can break a build.
package act

import (
	"errors"
	"fmt"
)

// Tag is a BER tag.
type Tag uint8

// The tags the LDAP subset uses.
const (
	TagBoolean     Tag = 0x01
	TagInteger     Tag = 0x02
	TagOctetString Tag = 0x04
	TagNull        Tag = 0x05
	TagEnumerated  Tag = 0x0a
	TagSequence    Tag = 0x30
	TagSet         Tag = 0x31
)

// maxElementBytes bounds the content of a single element.
//
// A length in a BER header is a claim, and a claim of four gigabytes must
// not turn into an allocation. The cap sits far above anything an LDAP
// message legitimately carries, and below anything that would hurt.
const maxElementBytes = 1 << 20

// errTruncated is returned for a message that ends in the middle of an
// element.
var errTruncated = errors.New("ber: the element is truncated")

// EncodeLength writes a BER length in the short and long forms.
//
// The short form covers 0 to 127 and the long form covers everything above
// it, in the fewest octets: a length is a claim about a buffer, and a padded
// claim is a lie the decoder has to undo.
func EncodeLength(n int) []byte {
	if n <= 0 {
		return []byte{0x00}
	}
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var buf [8]byte
	i := len(buf)
	for v := n; v > 0; v >>= 8 {
		i--
		buf[i] = byte(v)
	}
	// The leading zero octets are not part of the number: 65536 is two
	// octets, not three.
	for i < len(buf)-1 && buf[i] == 0 {
		i++
	}
	out := make([]byte, 0, 1+len(buf)-i)
	out = append(out, 0x80|byte(len(buf)-i))
	return append(out, buf[i:]...)
}

// EncodeElement writes one complete element: the tag, the length and the
// content.
func EncodeElement(tag Tag, content []byte) []byte {
	out := make([]byte, 0, 1+8+len(content))
	out = append(out, byte(tag))
	out = append(out, EncodeLength(len(content))...)
	return append(out, content...)
}

// Decode reads one element and returns the tag, the content and the rest.
//
// The indefinite length is refused. LDAP does not use it, and a decoder that
// accepted it would have to model the end-of-contents octets that close it,
// which is a second parser for a form nothing sends.
//
// The decoder never panics: every length is checked against the bytes that
// are actually present and against the element cap before anything is
// sliced.
func Decode(data []byte) (Tag, []byte, []byte, error) {
	if len(data) < 2 {
		return 0, nil, nil, fmt.Errorf("%w: %d bytes are not an element", errTruncated, len(data))
	}
	tag := Tag(data[0])
	if tag&0x1f == 0x1f {
		return 0, nil, nil, fmt.Errorf("ber: the high-tag-number form is not supported (tag byte 0x%02x)", data[0])
	}

	offset := 2
	length := 0
	lengthByte := data[1]
	switch {
	case lengthByte == 0x80:
		return 0, nil, nil, errors.New("ber: the indefinite length is not supported")
	case lengthByte < 0x80:
		length = int(lengthByte)
	default:
		count := int(lengthByte & 0x7f)
		if count > 4 {
			return 0, nil, nil, fmt.Errorf("ber: the length uses %d octets, over the 4-octet bound", count)
		}
		if len(data) < 2+count {
			return 0, nil, nil, fmt.Errorf("%w: the length needs %d octets", errTruncated, count)
		}
		for i := 0; i < count; i++ {
			length = length<<8 | int(data[2+i])
		}
		offset = 2 + count
	}

	if length > maxElementBytes {
		return 0, nil, nil, fmt.Errorf("ber: the content claims %d bytes, over the %d-byte bound", length, maxElementBytes)
	}
	if len(data) < offset+length {
		return 0, nil, nil, fmt.Errorf("%w: the content claims %d bytes and %d are present", errTruncated, length, len(data)-offset)
	}
	return tag, data[offset : offset+length], data[offset+length:], nil
}

// DecodeInteger reads an INTEGER content as an int64.
//
// The content is two's complement, and an INTEGER longer than eight bytes is
// refused rather than silently truncated: a value that does not fit is a
// value the caller has to hear about.
func DecodeInteger(content []byte) (int64, error) {
	if len(content) == 0 {
		return 0, errors.New("ber: the integer has no content")
	}
	if len(content) > 8 {
		return 0, fmt.Errorf("ber: the integer is %d bytes, over the 8-byte bound", len(content))
	}
	var value int64
	if content[0]&0x80 != 0 {
		// Sign extension: the leading bit set means the value is negative.
		value = -1
	}
	for _, b := range content {
		value = value<<8 | int64(b)
	}
	return value, nil
}

// DecodeString reads an OCTET STRING as a string.
func DecodeString(content []byte) string {
	return string(content)
}
