package permission

import (
	"context"
	"testing"
	"time"
)

func TestManagerWaitsForDecision(t *testing.T) {
	manager := NewManager()
	result := make(chan string, 1)
	go func() {
		decision, err := manager.Create(context.Background(), "session", "request", "write", time.Second, nil)
		if err != nil {
			result <- "error"
			return
		}
		result <- decision
	}()
	deadline := time.Now().Add(time.Second)
	for !manager.IsPending("session", "request") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !manager.Answer("session", "request", "allowOnce") {
		t.Fatal("expected pending permission to resolve")
	}
	select {
	case decision := <-result:
		if decision != "allowOnce" {
			t.Fatalf("decision = %q", decision)
		}
	case <-time.After(time.Second):
		t.Fatal("permission did not resolve")
	}
}

func TestManagerAlwaysAllow(t *testing.T) {
	manager := NewManager()
	manager.AllowAlways("read")
	decision, err := manager.Create(context.Background(), "session", "request", "read", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision != "allowAlways" {
		t.Fatalf("decision = %q", decision)
	}
}
