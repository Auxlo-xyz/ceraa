package miniapp

import (
	"net/http"
	"testing"

	"ceraa/harness/sessioncontrol"
)

func TestActorForUserUsesVerifiedTelegramID(t *testing.T) {
	actor, err := actorForUser(&User{ID: testUser}, testUser)
	if err != nil {
		t.Fatal(err)
	}
	if actor != (sessioncontrol.Actor{ID: "279058397", Source: "miniapp"}) {
		t.Fatalf("actor = %+v", actor)
	}
}

func TestActorForUserRejectsUnadmittedOrMissingUsers(t *testing.T) {
	for name, user := range map[string]*User{
		"missing":    nil,
		"wrong user": {ID: testUser + 1},
		"zero user":  {ID: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := actorForUser(user, testUser); err == nil {
				t.Fatal("accepted an invalid actor")
			}
		})
	}
}

func TestCancelActorCannotBeSelectedByRequestBody(t *testing.T) {
	server, _, _ := newFixture(t, testUser)
	initData := initDataFor(t, testToken, testUser, 0)
	server.cfg.SessionID = WebSessionID
	server.stopMu.Lock()
	called := false
	server.stop = func() { called = true }
	server.stopMu.Unlock()
	rec := postJSON(t, server.Handler(), "/api/sessions/"+WebSessionID+"/cancel", initData, `{"actor":"attacker","session_id":"other"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel with forged body actor = %d: %s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("cancel did not use the server-owned run")
	}
}
