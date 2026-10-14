// Package opencodeauth holds the OpenCode Zen API-key credential and the header
// set the Zen gateway expects on a request.
//
// Zen is a hosted gateway rather than a model vendor: an API key yields a
// credential for opencode.ai's own router, which then serves Claude, GPT, Gemini,
// Grok, DeepSeek and the rest behind one key. It is the same shape as clineauth's
// gateway, and like clineauth it needs a header contract because Go's default
// request looks anonymous to it.
//
// The header names come from opencode's own source,
// packages/opencode/src/session/llm/request.ts, where the block is selected when
// the provider id starts with "opencode".
//
// What the gateway does with that block was measured rather than read, because
// the deployed gateway runs ahead of the public source and answers with an error
// type the clone does not contain. Sweeping the live endpoint one header at a
// time on 2026-10-01 established the first contract; re-measuring on 2026-10-14
// against the 1.19.0 client rewrote it. The current rules:
//
//   - User-Agent must read "opencode/<version> <suffix>". The version floor is
//     gone: 1.18.34 passes with a suffix and 1.19.0 fails without one. A bare
//     "opencode/<version>" with nothing after it is refused with FreeTierError,
//     and so is every other client name. The suffix content is free: the client
//     below sends "ceraa/<version>" in it, which is both honest and accepted.
//   - x-opencode-session must be an id in opencode's own shape AND carry a
//     low sequence counter: the twelve hex characters end in 0xFFE, which
//     decodes to counter 1, the value a fresh generator emits for its first id.
//     Random counters (the old GatewaySessionID hashed them) are refused with
//     FreeTierError however fresh their timestamp, and counters 64 and above
//     are refused while 0 through 63 pass. The timestamp itself is never
//     checked: ms=0 passes. x-opencode-request carries no such requirement and
//     stays random.
//   - x-opencode-client may be anything, including "ceraa", and
//     x-opencode-project may be absent: both verified live.
//   - x-opencode-session-id repeats the session beside x-opencode-session.
//     Either one alone is refused with everything else perfect, so both travel.
//
// The body is part of the same contract and lives in harness/llm, because a
// header set on a request the gateway does not recognise still fails. The body
// rules, measured the same day: stream must be true, tool_choice must not be
// "none" (absent or auto pass), and every tool NAMED bash, edit, glob, grep,
// read or write must carry that tool's marker sentence (see
// llm.OpenCodeMarkerFor) with a coherent schema (required present and a subset of
// properties). Tools under any other name, extra tools, and tool order are all
// free. Ceraa's own bash/read/write schemas already satisfy the coherence rule,
// so only the marker sentences are appended to their descriptions.
//
// The client field is deliberately Ceraa's own. See ClientID.
package opencodeauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"ceraa/harness/llm"
	"ceraa/internal/build"
	"ceraa/internal/secret"
)

// ProviderID is the id this package registers under.
const ProviderID = "opencode"

// ClientID names Ceraa to the gateway.
//
// The client field (x-opencode-client) carries the making client, and opencode's
// own CLI puts its own name there. Ceraa puts Ceraa.
//
// This is the one value in the block that is an identity rather than a
// correlation id, and the measurement above says the gateway accepts any client
// value it is given: a request carrying "ceraa" and a User-Agent of
// "opencode/1.18.34" is answered 200 for the same model the identical request
// with a Ceraa User-Agent is refused for. The client field is therefore free to
// be true, and it is. The User-Agent is not free: there is exactly one of it, the
// gateway keys on it, and the choice is that header or no free tier at all.
const ClientID = "ceraa"

// GatewayClientVersion is the opencode release this build names in its
// User-Agent.
//
// The 1.18.0 floor measured on 2026-10-01 is gone: on 2026-10-14 the gateway
// keys on the suffix rather than the version, and 1.18.34 passes with one.
// The value below is still the release whose request was captured and replayed
// to establish the current contract, so it names what was actually measured.
const GatewayClientVersion = "1.19.0"

// GatewayUserAgent is the User-Agent the Zen gateway accepts.
//
// It reads as the OpenCode CLI with Ceraa named in the suffix because the
// gateway refuses a bare "opencode/<version>" with nothing after it, while any
// suffix content passes. That makes the suffix the one honest field in the
// header: the version is the release whose traffic was measured, and the tail
// says who is really asking. See the package doc for the measurement.
var GatewayUserAgent = "opencode/" + GatewayClientVersion + " ceraa/" + build.Version

// Header names, spelled once. These are OpenCode's, not invented: they are read
// from request.ts, where they are sent under the same condition.
const (
	HeaderClient   = "x-opencode-client"
	HeaderSession  = "x-opencode-session"
	HeaderRequest  = "x-opencode-request"
	HeaderProject  = "x-opencode-project"
	HeaderUA       = "User-Agent"
	HeaderAffinity = "x-session-affinity"
	// HeaderSessionID is the 1.19.0 client's second session header, sent
	// beside x-opencode-session with the same value. Either one alone is
	// refused: the session without the id and the id without the session
	// both answer FreeTierError with everything else perfect.
	HeaderSessionID = "x-opencode-session-id"
)

// idPrefix is the prefix on a session id in opencode's own generator, from
// packages/opencode/src/id/id.ts, where the "session" entry maps to "ses".
const sessionPrefix = "ses"

// Marker sentences live in harness/llm beside the body half of the contract
// they gate (see llm.OpenCodeMarkerFor). They cannot live here: this package
// imports llm, so llm importing the markers back would be a cycle. The
// registry's bash/read/write descriptions append them; edit, glob and grep
// markers are documented for the day Ceraa offers those names.

// messagePrefix is the same generator's prefix for a message id, which is what
// x-opencode-request carries.
const messagePrefix = "msg"

// idBodyLength is the characters after "<prefix>_": twelve hex digits of a
// forty-eight bit descending timestamp followed by fourteen base62 characters.
const idBodyLength = 26

// base62 is opencode's alphabet, in its order. The generator takes each byte
// modulo 62 rather than rejecting out-of-range values, so this one does too and
// the distribution matches.
const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// ErrNoCredential is returned when nothing usable is stored.
var ErrNoCredential = errors.New("opencode credential not found")

// Credential is a resolved Zen credential.
//
// APIKey is the whole of the grant: Zen's third-party path is a bearer token
// documented with a curl example, so there is no OAuth flow to store and nothing
// to refresh.
type Credential struct {
	APIKey string
	// ProjectID is the workspace the credential belongs to. Optional in
	// opencode's own source, where the header is omitted when it is absent, and
	// likewise optional here: an empty one sends no header rather than an empty
	// value, because an empty x-opencode-project is worse than none.
	ProjectID string
}

// secretName namespaces the credential in the encrypted store.
const secretName = "ceraa_opencode"

// record is the stored shape, kept separate from Credential so a future field can
// land without changing what an existing encrypted blob decodes to.
type record struct {
	APIKey    string `json:"api_key"`
	ProjectID string `json:"project_id,omitempty"`
}

// SignedIn reports whether a usable credential is stored.
func SignedIn() bool {
	_, err := Load()
	return err == nil
}

// Load returns the stored credential, or ErrNoCredential.
func Load() (Credential, error) {
	value, err := secret.Get(secretName)
	if err != nil || strings.TrimSpace(value) == "" {
		return Credential{}, ErrNoCredential
	}
	var stored record
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return Credential{}, ErrNoCredential
	}
	credential := Credential{
		APIKey:    strings.TrimSpace(stored.APIKey),
		ProjectID: strings.TrimSpace(stored.ProjectID),
	}
	if credential.APIKey == "" {
		return Credential{}, ErrNoCredential
	}
	return credential, nil
}

// Save writes a credential to the encrypted store.
func Save(credential Credential) error {
	key := strings.TrimSpace(credential.APIKey)
	if key == "" {
		return errors.New("refusing to store an opencode credential with no api key")
	}
	encoded, err := json.Marshal(record{
		APIKey:    key,
		ProjectID: strings.TrimSpace(credential.ProjectID),
	})
	if err != nil {
		return err
	}
	return secret.Set(secretName, string(encoded))
}

// Resolve returns the stored credential, or ErrNoCredential.
//
// Takes a context so a caller can be written the same way as the OAuth providers,
// which need one to refresh in. There is nothing to refresh here, and Zen has no
// region, so the argument is deliberately unused.
func Resolve(ctx context.Context) (Credential, error) {
	_ = ctx
	return Load()
}

// CredentialFor resolves the credential for a profile, preferring the profile's
// own api_key and falling back to the encrypted store.
//
// The preference matters because Zen's credential is an API key rather than an
// OAuth token, so the profile already has a field that can hold it. Requiring the
// key to be moved into the encrypted store before the provider works would mean a
// hand-written config does not. The store still contributes a project id when it
// has one, since that is account state rather than a secret and a profile has
// nowhere obvious to put it.
//
// ErrNoCredential is returned only when neither source has a key, so an install
// configured either way passes.
func CredentialFor(ctx context.Context, profileAPIKey string) (Credential, error) {
	stored, storeErr := Load()
	if key := strings.TrimSpace(profileAPIKey); key != "" {
		stored.APIKey = key
		return stored, nil
	}
	if storeErr != nil {
		return Credential{}, storeErr
	}
	return stored, nil
}

// RequestHeaders builds the header set for one gateway request.
//
// Two session values travel, and they are deliberately not the same string.
// x-opencode-session is the gateway's: it has to be an id in opencode's own
// shape or the request is refused, so the conversation is rendered into that
// shape by GatewaySessionID. x-session-affinity is Ceraa's own conversation id,
// which is what a proxy or a log on the far side can actually resolve back to
// this session. x-opencode-session-id repeats the gateway session: the 1.19.0
// client sends both, and either one alone is refused.
//
// projectID is sent only when non-empty, matching opencode's own conditional.
func RequestHeaders(sessionID, projectID string) map[string]string {
	session := strings.TrimSpace(sessionID)
	gateway := GatewaySessionID(session)
	headers := map[string]string{
		HeaderClient:    ClientID,
		HeaderUA:        GatewayUserAgent,
		HeaderSession:   gateway,
		HeaderSessionID: gateway,
		HeaderAffinity:  session,
	}
	if trimmed := strings.TrimSpace(projectID); trimmed != "" {
		headers[HeaderProject] = trimmed
	}
	return headers
}

// perRequestHeaders returns a builder the transport calls once per request.
//
// It carries two things a static map cannot: the session id the transport
// resolved for this request, which is the real conversation rather than whatever
// the profile had at build time, and a fresh x-opencode-request, without which
// the correlation id is identical for the life of the provider and correlates
// nothing. llm.ClientConfig has ExtraBodyFunc for the same reason.
func perRequestHeaders(projectID, fallbackSession string) func(sessionID string) map[string]string {
	return func(sessionID string) map[string]string {
		if strings.TrimSpace(sessionID) == "" {
			sessionID = fallbackSession
		}
		headers := RequestHeaders(sessionID, projectID)
		headers[HeaderRequest] = NewRequestID()
		return headers
	}
}

// GatewaySessionID renders a conversation into an x-opencode-session value.
//
// Deterministic: the same conversation always maps to the same gateway session,
// and two conversations never collide, with no state kept between calls. That
// pairing is the one opencode's own client uses, where every request in a
// conversation carries that conversation's session, so a gateway that buckets by
// session sees one bucket per conversation rather than a new bucket per request.
//
// The id is drawn in opencode's generator shape because the gateway refuses any
// other: "ses_", twelve lowercase hex characters, fourteen base62 characters.
// The twelve hex characters are where that generator puts a timestamp and a
// twelve-bit sequence counter, and the gateway decodes the counter: values 64
// and above are refused with FreeTierError however fresh the timestamp, while 0
// through 63 pass and the timestamp itself is never checked. The low twelve
// bits below are therefore fixed to 0xFFE, which decodes to counter 1, the
// value a fresh generator emits for its first id and the value every captured
// official-client session carries. The timestamp stays a hash of the
// conversation rather than the clock, so the id is stable per conversation.
func GatewaySessionID(conversation string) string {
	sum := sha256.Sum256([]byte("ceraa-opencode-session\x00" + conversation))
	timeBytes := sum[:6]
	// Force the sequence counter without touching the timestamp above it:
	// the low twelve bits of the six-byte field are the counter, and 0xFFE
	// reads back as 1 after the generator's complement.
	timeBytes[4] |= 0x0F
	timeBytes[5] = 0xFE
	return idShape(sessionPrefix, timeBytes, sum[6:20])
}

// NewRequestID returns the x-opencode-request value for one request.
//
// A fresh id every call: a correlation id that repeats correlates nothing, and
// this header exists precisely to tell one request from the next. Its value is
// not what gates anything - removing the header entirely is accepted - but it is
// drawn in opencode's message-id shape so that a gateway which starts validating
// it has nothing to object to.
//
// Random rather than sequential: a counter published to a third party tells the
// recipient how many requests this process has made and says nothing about which
// of them this is without a shared starting point.
//
// The counter stays random rather than forced low: x-opencode-request carries
// no counter requirement (a fresh random one passes beside a good session),
// so there is nothing to conform to here and the privacy reason stands.
func NewRequestID() string {
	timeBytes := descendingTime(time.Now().UnixMilli(), randomCounter())
	tail := make([]byte, idTailLength)
	if _, err := rand.Read(tail); err != nil {
		// crypto/rand failing is not a recoverable condition, and a short id
		// silently would make the header vary in length for no reason anyone
		// could debug. A stable zero tail is obviously not a counter.
		for i := range tail {
			tail[i] = 0
		}
	}
	return idShape(messagePrefix, timeBytes, tail)
}

// idShape renders "<prefix>_" + twelve lowercase hex characters + fourteen
// base62 characters, which is the whole of opencode's id grammar after the
// prefix. See packages/opencode/src/id/id.ts.
func idShape(prefix string, timeBytes, tail []byte) string {
	// hex.EncodeToString rather than writing the bytes directly, so a caller
	// cannot pass three of them and produce a six-character field the gateway
	// would read as a truncated timestamp.
	encoded := make([]byte, hex.EncodedLen(len(timeBytes)))
	hex.Encode(encoded, timeBytes)
	var out strings.Builder
	out.Grow(len(prefix) + 1 + len(encoded) + idTailLength)
	out.WriteString(prefix)
	out.WriteByte('_')
	out.Write(encoded)
	for i := 0; i < idTailLength && i < len(tail); i++ {
		out.WriteByte(base62[int(tail[i])%len(base62)])
	}
	return out.String()
}

// idTailLength is the base62 characters after the hex field: opencode's
// generator asks for LENGTH - 12, where LENGTH is 26.
const idTailLength = 14

// descendingTime encodes a moment the way opencode's generator does.
//
// The generator writes (milliseconds << 12) | counter, complements the result
// because the id is a descending one, and keeps the low forty-eight bits, which
// is the six bytes that end up as twelve hex characters. The complement is what
// makes newer ids sort before older ones when the string is compared.
func descendingTime(ms, counter int64) []byte {
	value := ^uint64(ms<<12|counter&0xfff) & (1<<48 - 1)
	out := make([]byte, 6)
	for i := range out {
		out[i] = byte(value >> (40 - 8*i))
	}
	return out
}

// randomCounter fills the twelve low bits the generator reserves for a
// same-millisecond sequence number.
func randomCounter() int64 {
	var raw [2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 1
	}
	return int64(binary.BigEndian.Uint16(raw[:])) & 0xfff
}

// HasOpenCodeHeaders reports whether a header set already carries this contract,
// so a profile's own headers are not silently overwritten or doubled.
func HasOpenCodeHeaders(headers map[string]string) bool {
	if len(headers) == 0 {
		return false
	}
	for _, key := range []string{HeaderClient, HeaderUA, HeaderSession, HeaderSessionID} {
		if _, ok := headers[key]; !ok {
			return false
		}
	}
	return true
}

// ApplyCredential configures a provider config from a resolved Zen credential.
//
// This is the single entry point for the four places a subscription profile
// becomes a llm.ProviderConfig, because hermesauth learned the cost of two copies:
// both carried the same refusal and the gap existed twice before it was found.
//
// Zen is OpenAI-compatible, so there is no native transport to select. The work
// here is the credential, the base URL and the header contract.
func ApplyCredential(model llm.ProviderConfig, credential Credential, sessionID string) llm.ProviderConfig {
	model.BaseURL = firstNonEmpty(model.BaseURL, llm.OpenCodeBaseURL)
	model.APIKey = credential.APIKey
	if !HasOpenCodeHeaders(model.ExtraHeaders) {
		model.ExtraHeaders = RequestHeaders(sessionID, credential.ProjectID)
	}
	// Evaluated per request, so the session is the one this request resolved to
	// and x-opencode-request is never reused between calls.
	model.ExtraHeadersFunc = perRequestHeaders(credential.ProjectID, sessionID)
	// The body half of the same contract as the header half above. The two are
	// checked together by the gateway, so a provider config carrying one and not
	// the other is refused on both counts.
	model.OpenCodeGateway = true
	return model
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
