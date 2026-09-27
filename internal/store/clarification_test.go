package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestClarificationRounds(t *testing.T) {
	s, c := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "asker", "handler", "dinner")
	question := envelope.Reply{Status: envelope.StatusNeedsInput, Body: "where?"}
	if _, err := s.Reply(ctx, req.ID, "handler", question); !errors.Is(err, ErrWrongState) {
		t.Fatalf("unclaimed: %v", err)
	}
	if _, err := s.Answer(ctx, req.ID, "asker", "here"); !errors.Is(err, ErrWrongState) {
		t.Fatalf("not waiting: %v", err)
	}
	for round := 1; round <= 3; round++ {
		if _, err := s.Claim(ctx, req.ID, "handler", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Reply(ctx, req.ID, "asker", question); !errors.Is(err, ErrForbidden) {
			t.Fatalf("wrong handler: %v", err)
		}
		if _, err := s.Reply(ctx, req.ID, "handler", question); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Reply(ctx, req.ID, "handler", question); !errors.Is(err, ErrWrongState) {
			t.Fatalf("duplicate question: %v", err)
		}
		c.advance(2 * time.Minute)
		if changes, err := s.Sweep(ctx); err != nil || len(changes) != 0 {
			t.Fatalf("paused lease: %+v %v", changes, err)
		}
		if _, err := s.Claim(ctx, req.ID, "handler", time.Minute); !errors.Is(err, ErrWrongState) {
			t.Fatalf("claim waiting: %v", err)
		}
		in, err := s.UnseenReplies(ctx, "asker")
		if err != nil || len(in) != 1 || len(in[0].Exchanges) != round || in[0].Done() {
			t.Fatalf("unseen = %+v %v", in, err)
		}
		if _, err := s.Answer(ctx, req.ID, "handler", "Nopa"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("wrong asker: %v", err)
		}
		resumed, err := s.Answer(ctx, req.ID, "asker", "Nopa")
		if err != nil || !resumed.Resumed || resumed.ID != req.ID || resumed.To != req.To || resumed.TraceID != req.TraceID || resumed.Hop != req.Hop || len(resumed.Exchanges) != round || resumed.Exchanges[round-1].Answer != "Nopa" {
			t.Fatalf("resume = %+v %v", resumed, err)
		}
		if _, err := s.Answer(ctx, req.ID, "asker", "duplicate"); !errors.Is(err, ErrWrongState) {
			t.Fatalf("duplicate answer: %v", err)
		}
		if n, err := s.CountUnseenReplies(ctx, "asker"); err != nil || n != 0 {
			t.Fatalf("stale question count: %d %v", n, err)
		}
		result, err := s.Get(ctx, req.ID, "asker")
		if err != nil || result.Reply != nil || result.Status != envelope.StatusQueued {
			t.Fatalf("stale reply: %+v %v", result, err)
		}
		inbox, err := s.Deliver(ctx, "handler", 1, time.Minute)
		if err != nil || len(inbox) != 1 || !inbox[0].Resumed || len(inbox[0].Exchanges) != round {
			t.Fatalf("delivery: %+v %v", inbox, err)
		}
	}
	if _, err := s.Claim(ctx, req.ID, "handler", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "handler", question); !errors.Is(err, ErrWrongState) {
		t.Fatalf("round cap: %v", err)
	}
	if _, err := s.Reply(ctx, req.ID, "handler", envelope.Reply{Status: envelope.StatusAnswered, Body: "booked"}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Get(ctx, req.ID, "asker")
	if err != nil || !result.Done() || len(result.Exchanges) != 3 {
		t.Fatalf("final: %+v %v", result, err)
	}
}

func TestClarificationExpiryAndRemoval(t *testing.T) {
	for _, action := range []string{"expiry", "remove sender", "remove target"} {
		t.Run(action, func(t *testing.T) {
			s, c := open(t, ":memory:")
			ctx := context.Background()
			req := ask(t, s, "asker", "handler", "dinner")
			if _, err := s.Claim(ctx, req.ID, "handler", time.Minute); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reply(ctx, req.ID, "handler", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "where?"}); err != nil {
				t.Fatal(err)
			}
			want := envelope.StatusCancelled
			if action == "expiry" {
				c.advance(time.Hour)
				if _, err := s.Answer(ctx, req.ID, "asker", "late"); !errors.Is(err, ErrWrongState) {
					t.Fatalf("late answer: %v", err)
				}
				if _, err := s.Sweep(ctx); err != nil {
					t.Fatal(err)
				}
				want = envelope.StatusExpired
			} else {
				name := "asker"
				if action == "remove target" {
					name = "handler"
				}
				if ids, err := s.CancelAllFor(ctx, name); err != nil || len(ids) != 1 {
					t.Fatalf("remove: %v %v", ids, err)
				}
			}
			_, status, err := s.Request(ctx, req.ID)
			if err != nil || status != want {
				t.Fatalf("state = %s %v", status, err)
			}
			if n, err := s.CountUnseenReplies(ctx, "asker"); err != nil || n != 0 {
				t.Fatalf("closed question wakes: %d %v", n, err)
			}
			if result, err := s.Get(ctx, req.ID, "asker"); err != nil || result.Reply != nil || len(result.Exchanges) != 1 {
				t.Fatalf("closed question: %+v %v", result, err)
			}
		})
	}
}

func TestClarificationRequiresLiveAskClaim(t *testing.T) {
	for _, scenario := range []string{"notify", "lease elapsed", "TTL elapsed"} {
		t.Run(scenario, func(t *testing.T) {
			s, c := open(t, ":memory:")
			kind := envelope.KindAsk
			if scenario == "notify" {
				kind = envelope.KindNotify
			}
			req, err := s.Enqueue(t.Context(), envelope.Request{From: "asker", To: "handler", Body: "dinner", Kind: kind}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			lease := time.Minute
			if scenario == "TTL elapsed" {
				lease = time.Hour
			}
			if _, err := s.Claim(t.Context(), req.ID, "handler", lease); err != nil {
				t.Fatal(err)
			}
			if scenario != "notify" {
				c.advance(time.Minute)
			}
			if _, err := s.Reply(t.Context(), req.ID, "handler", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "where?"}); !errors.Is(err, ErrWrongState) {
				t.Fatalf("invalid claim: %v", err)
			}
		})
	}
}

func TestClarificationSizeAndConcurrentAnswer(t *testing.T) {
	s, _ := open(t, ":memory:")
	ctx := context.Background()
	req := ask(t, s, "asker", "handler", "dinner")
	if _, err := s.Claim(ctx, req.ID, "handler", time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "  ", strings.Repeat("x", envelope.MaxInputBody+1)} {
		if _, err := s.Reply(ctx, req.ID, "handler", envelope.Reply{Status: envelope.StatusNeedsInput, Body: body}); err == nil {
			t.Fatal("bad question accepted")
		}
	}
	if _, err := s.Reply(ctx, req.ID, "handler", envelope.Reply{Status: envelope.StatusNeedsInput, Body: strings.Repeat("x", envelope.MaxInputBody)}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "  ", strings.Repeat("x", envelope.MaxInputBody+1)} {
		if _, err := s.Answer(ctx, req.ID, "asker", body); err == nil {
			t.Fatal("bad answer accepted")
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := s.Answer(ctx, req.ID, "asker", strings.Repeat("a", envelope.MaxInputBody))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	won := 0
	for err := range errs {
		if err == nil {
			won++
		} else if !errors.Is(err, ErrWrongState) {
			t.Fatal(err)
		}
	}
	if won != 1 {
		t.Fatalf("successful answers: %d", won)
	}
}

func TestClarificationMigrationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, _ := open(t, path)
	ctx := context.Background()
	req := ask(t, s, "asker", "handler", "dinner")
	for _, col := range []string{"exchanges", "lease_paused", "resumed"} {
		if _, err := s.db.Exec(`ALTER TABLE requests DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, _ = open(t, path)
	if _, err := s.Claim(ctx, req.ID, "handler", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, req.ID, "handler", envelope.Reply{Status: envelope.StatusNeedsInput, Body: "where?"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, _ = open(t, path)
	if agents, err := s.AgentsWithUnseenReplies(ctx); err != nil || len(agents) != 1 || agents[0] != "asker" {
		t.Fatalf("restart wake: %v %v", agents, err)
	}
	if req, err := s.Answer(ctx, req.ID, "asker", "Nopa"); err != nil || len(req.Exchanges) != 1 || req.Exchanges[0].Question != "where?" {
		t.Fatalf("restart answer: %+v %v", req, err)
	}
}
