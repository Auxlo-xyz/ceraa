package sessioncontrol

import "strings"

// Actor is the authenticated principal acting on a control surface. A session
// or request id is not an authority by itself.
type Actor struct {
	ID     string
	Source string
	ChatID int64
}

// ControlContext binds an operation to its authenticated actor.
type ControlContext struct {
	Actor Actor
}

func (a Actor) valid() bool {
	return strings.TrimSpace(a.ID) != ""
}
