package client_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestClarificationRefusesOldRelay(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			r := distServer(t, func(w http.ResponseWriter, req *http.Request) {
				if req.Method != "GET" || req.URL.Path != "/v1/capabilities" {
					t.Errorf("unexpected old relay call: %s %s", req.Method, req.URL.Path)
				}
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"attachments":true}`))
			})
			if _, err := r.Reply(t.Context(), "id", "where?", envelope.StatusNeedsInput); !errors.Is(err, client.ErrNeedsInputUnsupported) {
				t.Fatal(err)
			}
			if _, err := r.ReplyAttached(t.Context(), "id", "where?", envelope.StatusNeedsInput, nil); !errors.Is(err, client.ErrNeedsInputUnsupported) {
				t.Fatal(err)
			}
			if _, err := r.Answer(t.Context(), "id", "Nopa"); !errors.Is(err, client.ErrNeedsInputUnsupported) {
				t.Fatal(err)
			}
		})
	}
}

func TestClarificationClientContext(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	asker, handler := m.Client(t, "grokbot"), m.Client(t, "muse")
	ctx := t.Context()
	req, err := asker.Send(ctx, "muse", "book dinner", envelope.KindAsk, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler.Claim(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.Reply(ctx, req.ID, "which restaurant?", envelope.StatusNeedsInput); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	result, err := asker.Get(ctx, req.ID, time.Second)
	if err != nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("get did not return question promptly: %v", err)
	}
	if result.Done() || len(result.Exchanges) != 1 {
		t.Fatalf("question = %+v", result)
	}
	for _, text := range []string{client.FormatResult(result), client.FormatReply(result)} {
		if !strings.Contains(text, "muse needs more information") || !strings.Contains(text, "tincan answer "+req.ID) {
			t.Fatalf("render = %s", text)
		}
	}
	if _, err := asker.Answer(ctx, req.ID, "Nopa, 2 people"); err != nil {
		t.Fatal(err)
	}
	in, err := handler.Poll(ctx, 0)
	if err != nil || len(in.Requests) != 1 {
		t.Fatalf("poll: %+v %v", in, err)
	}
	text := client.FormatRequest(in.Requests[0])
	for _, want := range []string{"Resumed", "book dinner", "which restaurant?", "Nopa, 2 people"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
}
