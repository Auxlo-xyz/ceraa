package freebuff

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"runtime"
	"strings"
)

// fingerprintPrefix matches the Freebuff CLI's legacy fingerprint format. The
// CLI's enhanced fingerprint embeds process.version and systeminformation
// output, which Go cannot reproduce byte-for-byte, so we mirror the legacy
// shape instead. The server treats the fingerprint as an anti-multi-account
// signal, so the value must be stable across restarts (the CLI regenerates a
// random legacy value per process, which is a defect we deliberately do not
// copy).
const fingerprintPrefix = "codebuff-cli-"

// newFingerprintID derives a stable device ID from machine facts that do not
// change between process launches. hostname+os+arch is a good enough stable
// seed: it is not secret, and the resulting value is only an opaque tag.
func newFingerprintID() string {
	var b strings.Builder
	host, _ := os.Hostname()
	b.WriteString(host)
	b.WriteString("|")
	b.WriteString(runtime.GOOS)
	b.WriteString("|")
	b.WriteString(runtime.GOARCH)
	sum := sha256.Sum256([]byte(b.String()))
	// 8 base64url characters, matching the CLI's slice length.
	encoded := base64.RawURLEncoding.EncodeToString(sum[:])
	if len(encoded) > 8 {
		encoded = encoded[:8]
	}
	return fingerprintPrefix + encoded
}

// fingerprintSeed is the set of machine facts hashed into the device ID,
// exposed for tests.
func fingerprintSeed() string {
	host, _ := os.Hostname()
	return host + "|" + runtime.GOOS + "|" + runtime.GOARCH
}
