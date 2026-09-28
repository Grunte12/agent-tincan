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
	old := PongRetry
	PongRetry = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { PongRetry = old })
	f := &flakyPonger{}
	_, retry, _ := AnswerPings(t.Context(), f, Inbox{Requests: []envelope.Request{{ID: "ping", Kind: envelope.KindPing}}}, "test")
	retry(t.Context())
	if f.replies != 2 {
		t.Fatalf("replies = %d, want a retry after the failure", f.replies)
	}
}

type failedPonger struct{ replies int }

func (f *failedPonger) Claim(context.Context, string) (envelope.Request, error) {
	return envelope.Request{}, nil
}
func (f *failedPonger) Reply(context.Context, string, string, envelope.Status) (envelope.Reply, error) {
	f.replies++
	return envelope.Reply{}, &APIError{Code: 503}
}
func TestAnswerPingsReturnsWorkBeforeRetries(t *testing.T) {
	old := PongRetry
	PongRetry = []time.Duration{time.Second}
	t.Cleanup(func() { PongRetry = old })
	f := &failedPonger{}
	start := time.Now()
	in, retry, err := AnswerPings(t.Context(), f, Inbox{Requests: []envelope.Request{
		{ID: "ping", Kind: envelope.KindPing}, {ID: "work", Kind: envelope.KindAsk},
	}}, "test")
	if err == nil || len(in.Requests) != 1 || in.Requests[0].ID != "work" || f.replies != 1 || time.Since(start) >= time.Second {
		t.Fatalf("ordinary work delayed or lost: %+v, %v, replies=%d", in, err, f.replies)
	}
	PongRetry = []time.Duration{0, 0, 0}
	retry(t.Context())
	if f.replies != 4 {
		t.Fatalf("replies=%d", f.replies)
	}
}

func TestRetryPongsStaysWithinCallerDeadline(t *testing.T) {
	// A distant caller deadline leaves the retries' own 30s bound.
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(time.Hour))
	defer cancel()
	var got time.Time
	RetryPongs(ctx, func(ctx context.Context) { got, _ = ctx.Deadline() })
	if d := time.Until(got); d > 30*time.Second || d < 25*time.Second {
		t.Fatalf("retry deadline in %v, want 30s", d)
	}

	// A nearer one (tincan wait --timeout) bounds them.
	near := time.Now().Add(time.Second)
	ctx, cancel = context.WithDeadline(t.Context(), near)
	defer cancel()
	RetryPongs(ctx, func(ctx context.Context) { got, _ = ctx.Deadline() })
	if !got.Equal(near) {
		t.Fatalf("retry deadline = %v, want the caller's %v", got, near)
	}

	// Without a caller deadline, retries outlive a cancelled caller, up to
	// their own bound.
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	var alive bool
	RetryPongs(ctx, func(ctx context.Context) {
		got, _ = ctx.Deadline()
		alive = ctx.Err() == nil
	})
	if !alive || time.Until(got) > 30*time.Second || time.Until(got) < 25*time.Second {
		t.Fatalf("detached retry: alive=%v deadline in %v", alive, time.Until(got))
	}
}
