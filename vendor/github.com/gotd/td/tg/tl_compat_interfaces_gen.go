// Helper definitions restored verbatim from the upstream generated tg package
// (qrlogin support that is normally emitted by the handlers generator).

package tg

import (
	"context"
)

// Entities is a container for entities (users, chats and channels) collected
// from an updates batch.
type Entities struct {
	Short    bool
	Users    map[int64]*User
	Chats    map[int64]*Chat
	Channels map[int64]*Channel
}

// LoginTokenHandler is a LoginToken event handler.
type LoginTokenHandler func(ctx context.Context, e Entities, update *UpdateLoginToken) error
