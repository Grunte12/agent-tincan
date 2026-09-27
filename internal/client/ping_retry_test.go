package client

import (
	"context"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

type flakyPonger struct{ replies int }

func (f *flakyPonger) Claim(context.Context, string) (envelope.Request, error) {
	return envelope.Request{}, nil
}

func (f *flakyPonger) Reply(context.Context, string, string, envelope.Status) (envelope.Reply, error) {
	f.replies++
	if f.replies == 1 {
		return envelope.Reply{}, &APIError{Code: 503, Message: "relay busy"}
	}
	return envelope.Reply{}, nil
}

func TestAnswerPingsRetriesAFailedPong(t *testing.T) {
	old := pongRetry
	pongRetry = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { pongRetry = old })
	f := &flakyPonger{}
	if _, err := AnswerPings(t.Context(), f, Inbox{Requests: []envelope.Request{{ID: "ping", Kind: envelope.KindPing}}}, "test"); err != nil {
		t.Fatalf("a pong that succeeds on retry reported %v", err)
	}
	if f.replies != 2 {
		t.Fatalf("replies = %d, want a retry after the failure", f.replies)
	}
}
