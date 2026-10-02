// Package miniapp verifies the initData a Telegram Mini App is launched with.
//
// Verification is deliberately a pure function of a string, a bot token and a
// clock. Nothing here reads a request, binds a socket or holds state, so the
// whole security decision can be pinned by a test vector taken from Telegram's
// own documentation before any server exists that could call it.
//
// The check is the HMAC scheme from
// https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app:
//
//	secret_key = HMAC_SHA256(key="WebAppData", message=bot_token)
//	hash       = HMAC_SHA256(key=secret_key,   message=data_check_string)
//
// where data_check_string is every received field except hash, sorted
// alphabetically and joined as key=value with a line feed. The signature field
// that Telegram now also sends is deliberately left in: only hash is removed,
// which matches Telegram's own reference implementation and the wording of the
// document above. Excluding it would mean accepting a payload whose signature
// column had been rewritten after the fact.
package miniapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors. Every rejection path returns one of these wrapped, so a
// caller can tell an unauthenticated request from a stale one without parsing
// a message.
var (
	// ErrNoToken means verification was attempted without a bot token. The
	// verifier fails closed: a missing token must never mean "accept".
	ErrNoToken = errors.New("no bot token")

	// ErrMalformed means the payload is not a query string, is unreadable, or
	// does not carry a field it must have.
	ErrMalformed = errors.New("init data is malformed")

	// ErrMissingHash means the payload carries no signature to check against.
	ErrMissingHash = errors.New("init data carries no hash")

	// ErrBadSignature means the recomputed hash does not match the one sent.
	ErrBadSignature = errors.New("init data signature does not match")

	// ErrExpired means auth_date is unreadable or outside the permitted window.
	ErrExpired = errors.New("init data is expired")

	// ErrNoUser means the payload verified but names nobody, so there is no
	// identity to authorise against.
	ErrNoUser = errors.New("init data carries no user")
)

// clockSkew is how far ahead of now auth_date may sit before it is treated as
// forged rather than as a badly set clock. Telegram has no future expiry, so a
// large forward timestamp is the way to make a payload live forever.
const clockSkew = time.Minute

// User is the identity a verified payload names.
type User struct {
	ID           int64
	Username     string
	FirstName    string
	LastName     string
	LanguageCode string
	AuthDate     time.Time
}

// Verify checks initData against botToken and returns the user it names.
//
// maxAge bounds how old auth_date may be. Zero or less skips that check, which
// is only ever correct in a test: a real caller must bound the window, or a
// payload captured once is good forever.
func Verify(initData, botToken string, maxAge time.Duration, now time.Time) (*User, error) {
	if botToken == "" {
		return nil, ErrNoToken
	}
	if initData == "" {
		return nil, ErrMalformed
	}
	fields, err := url.ParseQuery(initData)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	hash := fields.Get("hash")
	if hash == "" {
		return nil, ErrMissingHash
	}
	if !hmac.Equal([]byte(hashOf(botToken, dataCheckString(fields))), []byte(strings.ToLower(hash))) {
		return nil, ErrBadSignature
	}
	// Checked only after the signature, so an unauthenticated caller learns
	// nothing from which of the two a payload failed on.
	if err := checkFreshness(fields, maxAge, now); err != nil {
		return nil, err
	}
	user, err := parseUser(fields.Get("user"))
	if err != nil {
		return nil, err
	}
	if raw := fields.Get("auth_date"); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
			user.AuthDate = time.Unix(seconds, 0)
		}
	}
	return user, nil
}

// dataCheckString builds the string Telegram signed. Every field is included
// except hash, which is the value being checked; the rest are sorted and joined
// as key=value on a line feed.
func dataCheckString(fields url.Values) string {
	pairs := make([]string, 0, len(fields))
	for key, values := range fields {
		if key == "hash" || len(values) == 0 {
			continue
		}
		pairs = append(pairs, key+"="+values[0])
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\n")
}

// hashOf returns the hex HMAC of check with the secret derived from the token.
//
// The secret is raw bytes. Encoding it to hex first is a documented mistake in
// this space: the key Telegram derives is fed to the second HMAC as it stands.
func hashOf(botToken, check string) string {
	mac := hmac.New(sha256.New, []byte("WebAppData"))
	mac.Write([]byte(botToken))
	secret := mac.Sum(nil)

	mac = hmac.New(sha256.New, secret)
	mac.Write([]byte(check))
	return hex.EncodeToString(mac.Sum(nil))
}

// checkFreshness rejects payloads outside the permitted auth_date window.
func checkFreshness(fields url.Values, maxAge time.Duration, now time.Time) error {
	if maxAge <= 0 {
		return nil
	}
	raw := fields.Get("auth_date")
	if raw == "" {
		return ErrMalformed
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return ErrExpired
	}
	authDate := time.Unix(seconds, 0)
	age := now.Sub(authDate)
	if age > maxAge || age < -clockSkew {
		return ErrExpired
	}
	return nil
}

// parseUser decodes the JSON object Telegram nests in the user field.
func parseUser(raw string) (*User, error) {
	if raw == "" {
		return nil, ErrNoUser
	}
	var payload struct {
		ID           int64  `json:"id"`
		Username     string `json:"username"`
		FirstName    string `json:"first_name"`
		LastName     string `json:"last_name"`
		LanguageCode string `json:"language_code"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("%w: user is not JSON", ErrMalformed)
	}
	if payload.ID == 0 {
		return nil, ErrNoUser
	}
	return &User{
		ID:           payload.ID,
		Username:     payload.Username,
		FirstName:    payload.FirstName,
		LastName:     payload.LastName,
		LanguageCode: payload.LanguageCode,
	}, nil
}
