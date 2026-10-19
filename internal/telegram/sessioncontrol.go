package telegram

import (
	"context"
	"fmt"

	"ceraa/harness/event"
	"ceraa/harness/permission"
	"ceraa/harness/question"
	"ceraa/harness/sessioncontrol"
	"ceraa/harness/sessionstore"
)

// telegramControl builds the shared control facade around the stores and
// managers already owned by this process. It does not keep another run map.
func telegramControl(store *sessionstore.Store) (sessioncontrol.Service, error) {
	return sessioncontrol.New(sessioncontrol.Dependencies{
		Store:      store,
		Bus:        event.Default,
		Permission: permission.Default,
		Question:   question.Default,
	})
}

func pendingDecisionCount(store *sessionstore.Store, sessionID string) int {
	control, err := telegramControl(store)
	if err != nil {
		return 0
	}
	pending, err := control.PendingDecisions(context.Background(), sessioncontrol.PendingDecisionsRequest{
		SessionID: sessioncontrol.SessionID(sessionID),
	})
	if err != nil {
		return 0
	}
	return len(pending.Decisions)
}

// telegramActor is the actor a Telegram surface acts as. It matches the actor
// the run itself is started under (bot.go), so a button answer authorizes
// against the same identity that owns the run rather than inheriting the
// zero-value request the button tap arrives in.
func telegramActor(chatID int64) sessioncontrol.ControlContext {
	return sessioncontrol.ControlContext{Actor: sessioncontrol.Actor{
		ID:     fmt.Sprintf("telegram:%d", chatID),
		Source: "telegram",
		ChatID: chatID,
	}}
}

func answerTelegramPermission(store *sessionstore.Store, sessionID, requestID, decision string, chatID int64) bool {
	control, err := telegramControl(store)
	if err != nil {
		return false
	}
	_, err = control.AnswerPermission(context.Background(), sessioncontrol.AnswerPermissionRequest{
		SessionID: sessioncontrol.SessionID(sessionID), RequestID: requestID, Decision: decision,
		Context: telegramActor(chatID),
	})
	return err == nil
}

func answerTelegramQuestion(store *sessionstore.Store, sessionID, requestID, answer string, chatID int64) bool {
	control, err := telegramControl(store)
	if err != nil {
		return false
	}
	_, err = control.AnswerQuestion(context.Background(), sessioncontrol.AnswerQuestionRequest{
		SessionID: sessioncontrol.SessionID(sessionID), RequestID: requestID, Answer: answer,
		Context: telegramActor(chatID),
	})
	return err == nil
}
