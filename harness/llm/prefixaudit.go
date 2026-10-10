package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// PrefixAuditWindow is the number of leading bytes covered by Fingerprint.
// Providers key prefix cache on the start of the request body, so hashing
// only the head keeps the check cheap while still catching the breaks that
// matter. The total length is folded in separately so a pure tail append
// past the window still changes the fingerprint.
const PrefixAuditWindow = 32 * 1024

// Fingerprint returns a cheap byte-stability fingerprint of an outbound
// provider request body: hex sha256 of the first 32KB plus total length.
//
// Format is "<hex>:<len>", for example "e3b0c44...:0". It is pure: no
// logging, no I/O, safe on nil and empty input.
func Fingerprint(body []byte) string {
	head := body
	if len(head) > PrefixAuditWindow {
		head = head[:PrefixAuditWindow]
	}
	sum := sha256.Sum256(head)
	return hex.EncodeToString(sum[:]) + ":" + strconv.Itoa(len(body))
}

// CompareFingerprints reports whether two fingerprints describe a stable
// prefix. It returns "" when the prefix is stable, else a one-line
// human-readable description of what changed. It works on fingerprints
// only, so callers store one string per turn. It is pure: no logging.
func CompareFingerprints(prev, cur string) string {
	if prev == cur {
		return ""
	}
	prevHash, prevLen, prevOK := splitFingerprint(prev)
	curHash, curLen, curOK := splitFingerprint(cur)
	if !prevOK || !curOK {
		return fmt.Sprintf("prefix fingerprint changed: unparseable fingerprint (prev %q cur %q)", prev, cur)
	}
	hashChanged := prevHash != curHash
	lenChanged := prevLen != curLen
	switch {
	case hashChanged && !lenChanged:
		return fmt.Sprintf("early bytes changed within first %d bytes (length unchanged at %d)", PrefixAuditWindow, curLen)
	case !hashChanged && lenChanged:
		return fmt.Sprintf("prefix stable but length changed from %d to %d (change past first %d bytes)", prevLen, curLen, PrefixAuditWindow)
	default:
		return fmt.Sprintf("prefix bytes changed within first %d bytes and length changed from %d to %d", PrefixAuditWindow, prevLen, curLen)
	}
}

func splitFingerprint(fp string) (hash string, length int, ok bool) {
	sep := strings.LastIndex(fp, ":")
	if sep < 0 {
		return "", 0, false
	}
	hash = fp[:sep]
	if len(hash) != 64 {
		return "", 0, false
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(fp[sep+1:])
	if err != nil || n < 0 {
		return "", 0, false
	}
	return hash, n, true
}
