package miniapp

import (
	"context"
	"errors"
	"strconv"

	"ceraa/harness/sessioncontrol"
)

var errInvalidMiniAppActor = errors.New("invalid Mini App actor")

type actorContextKey struct{}

// actorForUser turns only the verified Telegram user into a control actor.
// Session IDs, including WebSessionID, identify resources and are never actors.
func actorForUser(user *User, allowID int64) (sessioncontrol.Actor, error) {
	if user == nil || user.ID <= 0 || allowID <= 0 || user.ID != allowID {
		return sessioncontrol.Actor{}, errInvalidMiniAppActor
	}
	return sessioncontrol.Actor{
		ID:     strconv.FormatInt(user.ID, 10),
		Source: "miniapp",
	}, nil
}

func withActor(ctx context.Context, actor sessioncontrol.Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func actorFromContext(ctx context.Context) (sessioncontrol.Actor, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(sessioncontrol.Actor)
	return actor, ok && actor.ID != ""
}
