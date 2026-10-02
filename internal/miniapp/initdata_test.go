package miniapp

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The documented vector below is copied from Telegram's own worked example, as
// reproduced by the Telegram-Mini-Apps project: it fixes the token, the three
// signed fields and the resulting hash. Every other test in this file may build
// payloads with hashOf, because this one pins hashOf against an answer computed
// somewhere else.
const (
	vectorToken  = "5768337691:AAH5YkoiEuPk8-FZa32hStHTqXiLPtAEhx8"
	vectorHash   = "c501b71e775f74ce10e377dea85a7ea24ecd640b223ea86dfe453e0eaed2e2b2"
	vectorAuth   = "1662771648"
	vectorQuery  = "AAHdF6IQAAAAAN0XohDhrOrc"
	vectorUser   = `{"id":279058397,"first_name":"Vladislav","last_name":"Kibenko","username":"vdkfrost","language_code":"ru","is_premium":true}`
	vectorMoment = 1662771648
)

// encodePairs renders key=value pairs in the order given, percent-encoded the
// way a Telegram client encodes them.
func encodePairs(pairs ...[2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, url.QueryEscape(pair[0])+"="+url.QueryEscape(pair[1]))
	}
	return strings.Join(parts, "&")
}

// vectorPayload is the documented payload in Telegram's field order.
func vectorPayload() string {
	return encodePairs(
		[2]string{"auth_date", vectorAuth},
		[2]string{"query_id", vectorQuery},
		[2]string{"user", vectorUser},
		[2]string{"hash", vectorHash},
	)
}

// TestVerifyAcceptsTelegramDocumentedVector is the whole point of this file:
// the algorithm must reproduce an answer Telegram published, rather than an
// answer this package computes for itself.
func TestVerifyAcceptsTelegramDocumentedVector(t *testing.T) {
	now := time.Unix(vectorMoment, 0)
	user, err := Verify(vectorPayload(), vectorToken, time.Hour, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if user.ID != 279058397 {
		t.Errorf("ID = %d, want 279058397", user.ID)
	}
	if user.Username != "vdkfrost" {
		t.Errorf("Username = %q, want vdkfrost", user.Username)
	}
	if user.FirstName != "Vladislav" || user.LastName != "Kibenko" {
		t.Errorf("name = %q %q, want Vladislav Kibenko", user.FirstName, user.LastName)
	}
	if user.LanguageCode != "ru" {
		t.Errorf("LanguageCode = %q, want ru", user.LanguageCode)
	}
	if user.AuthDate.Unix() != vectorMoment {
		t.Errorf("AuthDate = %d, want %d", user.AuthDate.Unix(), vectorMoment)
	}
}

// TestVerifySortsFieldsRegardlessOfArrivalOrder proves the check string is
// rebuilt rather than reused. The published hash was computed over the sorted
// form, so a payload arriving in a different order still verifies only if this
// sorts, and would fail if it trusted the order it was handed.
func TestVerifySortsFieldsRegardlessOfArrivalOrder(t *testing.T) {
	shuffled := encodePairs(
		[2]string{"user", vectorUser},
		[2]string{"hash", vectorHash},
		[2]string{"auth_date", vectorAuth},
		[2]string{"query_id", vectorQuery},
	)
	now := time.Unix(vectorMoment, 0)
	if _, err := Verify(shuffled, vectorToken, time.Hour, now); err != nil {
		t.Fatalf("Verify on a reordered payload: %v", err)
	}
}

// TestVerifyRejectsATamperedField covers the case the signature exists for.
func TestVerifyRejectsATamperedField(t *testing.T) {
	tampered := encodePairs(
		[2]string{"auth_date", vectorAuth},
		[2]string{"query_id", vectorQuery},
		[2]string{"user", strings.Replace(vectorUser, "vdkfrost", "someone-else", 1)},
		[2]string{"hash", vectorHash},
	)
	now := time.Unix(vectorMoment, 0)
	if _, err := Verify(tampered, vectorToken, time.Hour, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify error = %v, want ErrBadSignature", err)
	}
}

// TestVerifyRejectsADifferentToken checks the secret really comes from the
// token rather than from some constant baked into the package.
func TestVerifyRejectsADifferentToken(t *testing.T) {
	now := time.Unix(vectorMoment, 0)
	if _, err := Verify(vectorPayload(), "9999999999:AAIncorrectTokenForTesting", time.Hour, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify error = %v, want ErrBadSignature", err)
	}
}

// TestVerifyFailsClosedWithoutAToken pins the one refusal that must never
// become an acceptance, since a missing credential is a configuration state
// rather than a hostile payload.
func TestVerifyFailsClosedWithoutAToken(t *testing.T) {
	now := time.Unix(vectorMoment, 0)
	if _, err := Verify(vectorPayload(), "", time.Hour, now); !errors.Is(err, ErrNoToken) {
		t.Fatalf("Verify error = %v, want ErrNoToken", err)
	}
}

// TestVerifyRejectsPayloadsWithoutAHash covers a payload that carries data but
// no signature at all.
func TestVerifyRejectsPayloadsWithoutAHash(t *testing.T) {
	unsigned := encodePairs(
		[2]string{"auth_date", vectorAuth},
		[2]string{"user", vectorUser},
	)
	now := time.Unix(vectorMoment, 0)
	if _, err := Verify(unsigned, vectorToken, time.Hour, now); !errors.Is(err, ErrMissingHash) {
		t.Fatalf("Verify error = %v, want ErrMissingHash", err)
	}
}

// TestVerifyRejectsEmptyAndUnreadablePayloads separates the two refusals that
// both mean "this string is not initData".
func TestVerifyRejectsEmptyAndUnreadablePayloads(t *testing.T) {
	now := time.Unix(vectorMoment, 0)
	if _, err := Verify("", vectorToken, time.Hour, now); !errors.Is(err, ErrMalformed) {
		t.Errorf("empty payload error = %v, want ErrMalformed", err)
	}
	if _, err := Verify("user=%zz", vectorToken, time.Hour, now); !errors.Is(err, ErrMalformed) {
		t.Errorf("bad escape error = %v, want ErrMalformed", err)
	}
}

// TestVerifyRejectsStaleAndFuturePayloads covers both ends of the window. The
// future case matters more than it looks: without it a payload stamped far
// ahead never ages out and stays valid indefinitely.
func TestVerifyRejectsStaleAndFuturePayloads(t *testing.T) {
	authDate := time.Unix(vectorMoment, 0)
	if _, err := Verify(vectorPayload(), vectorToken, time.Hour, authDate.Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Errorf("stale payload error = %v, want ErrExpired", err)
	}
	if _, err := Verify(vectorPayload(), vectorToken, time.Hour, authDate.Add(-24*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Errorf("future payload error = %v, want ErrExpired", err)
	}
}

// TestVerifyWithoutAMaxAgeSkipsTheWindow documents that zero means "no freshness
// check at all". Only a test should pass zero, and the comment on Verify says so.
func TestVerifyWithoutAMaxAgeSkipsTheWindow(t *testing.T) {
	authDate := time.Unix(vectorMoment, 0)
	if _, err := Verify(vectorPayload(), vectorToken, 0, authDate.Add(10*365*24*time.Hour)); err != nil {
		t.Fatalf("Verify with no max age: %v", err)
	}
}

// TestVerifyRejectsAnUnreadableAuthDate keeps a payload with a signature over a
// field it cannot parse from being treated as fresh.
func TestVerifyRejectsAnUnreadableAuthDate(t *testing.T) {
	payload := signFields(t, "not-a-timestamp", vectorUser)
	if _, err := Verify(payload, vectorToken, time.Hour, time.Unix(vectorMoment, 0)); !errors.Is(err, ErrExpired) {
		t.Fatalf("Verify error = %v, want ErrExpired", err)
	}
}

// TestVerifyRequiresAUser keeps a signature-valid payload with no identity from
// reaching an authorisation decision with nothing to decide.
func TestVerifyRequiresAUser(t *testing.T) {
	payload := signFields(t, vectorAuth, "")
	if _, err := Verify(payload, vectorToken, time.Hour, time.Unix(vectorMoment, 0)); !errors.Is(err, ErrNoUser) {
		t.Fatalf("Verify error = %v, want ErrNoUser", err)
	}
}

// TestVerifyRejectsAUserThatIsNotJSON separates a signed payload naming a user
// in a shape Telegram would never send from one that simply omits the field.
func TestVerifyRejectsAUserThatIsNotJSON(t *testing.T) {
	payload := signFields(t, vectorAuth, "not json")
	if _, err := Verify(payload, vectorToken, time.Hour, time.Unix(vectorMoment, 0)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify error = %v, want ErrMalformed", err)
	}
}

// TestDataCheckStringShape pins the exact bytes that get signed, which is the
// part an accidental change would silently break: drop one field, reorder, or
// swap the separator and every real payload stops verifying while the tests
// above still pass, because they build payloads through the same helper.
func TestDataCheckStringShape(t *testing.T) {
	fields, err := url.ParseQuery("hash=deadbeef&signature=sig&chat_instance=42&auth_date=1662771648")
	if err != nil {
		t.Fatal(err)
	}
	got := dataCheckString(fields)
	want := "auth_date=1662771648\nchat_instance=42\nsignature=sig"
	if got != want {
		t.Fatalf("dataCheckString =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "hash") {
		t.Error("dataCheckString still contains the hash field it is checked against")
	}
}

// signFields builds a payload the way a Telegram client would, signed with the
// real algorithm. It exists for cases where the shape of the payload, rather
// than the correctness of the signature, is what is under test.
func signFields(t *testing.T, authDate, user string) string {
	t.Helper()
	query := url.Values{}
	query.Set("auth_date", authDate)
	if user != "" {
		query.Set("user", user)
	}
	query.Set("hash", hashOf(vectorToken, dataCheckString(query)))
	return query.Encode()
}
