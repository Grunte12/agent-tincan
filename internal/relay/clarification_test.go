package relay

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestClarificationRoundTrip(t *testing.T) {
	h := newHarness(t, Config{})
	rec := &replyRecorder{}
	h.srv.SetEvents(rec)
	req := h.send(grokAddr, "muse", "book dinner")
	base := "/v1/requests/" + req.ID
	h.do(museAddr, "POST", base+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", base+"/reply", `{"status":"needs_input","body":"which restaurant?"}`, http.StatusOK, nil)
	var result envelope.Result
	h.do(grokAddr, "GET", base, "", http.StatusOK, &result)
	if result.Done() || result.Reply == nil || result.Reply.Body != "which restaurant?" {
		t.Fatalf("question = %+v", result)
	}
	h.do(instinctAddr, "POST", base+"/answer", `{"body":"wrong"}`, http.StatusForbidden, nil)
	h.do(grokAddr, "POST", base+"/answer", `{"body":"Nopa, 2 people"}`, http.StatusOK, nil)
	h.do(museAddr, "POST", base+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", base+"/reply", `{"body":"booked"}`, http.StatusOK, nil)
	h.do(grokAddr, "GET", base, "", http.StatusOK, &result)
	if !result.Done() || result.Reply.Body != "booked" {
		t.Fatalf("final = %+v", result)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.replied) != 2 {
		t.Fatalf("reply wake events = %d", len(rec.replied))
	}
}

func TestClarificationHTTPGuardsAndAudit(t *testing.T) {
	h := newHarness(t, Config{})
	req := h.send(grokAddr, "muse", "book dinner")
	base := "/v1/requests/" + req.ID
	question := `{"status":"needs_input","body":"private question"}`
	h.do(museAddr, "POST", base+"/reply", question, http.StatusConflict, nil)
	h.do(grokAddr, "POST", base+"/answer", `{"body":"too early"}`, http.StatusConflict, nil)
	h.do(museAddr, "POST", base+"/claim", "", http.StatusOK, nil)
	h.do(instinctAddr, "POST", base+"/reply", question, http.StatusForbidden, nil)
	for _, body := range []string{"", "  ", strings.Repeat("x", envelope.MaxInputBody+1)} {
		raw, _ := json.Marshal(map[string]string{"status": "needs_input", "body": body})
		h.do(museAddr, "POST", base+"/reply", string(raw), http.StatusBadRequest, nil)
	}
	h.do(museAddr, "POST", base+"/reply", `{"status":"needs_input","body":"where?","attachments":[{"id":"x"}]}`, http.StatusBadRequest, nil)
	for range 3 {
		h.do(museAddr, "POST", base+"/reply", question, http.StatusOK, nil)
		for _, body := range []string{"", "  ", strings.Repeat("x", envelope.MaxInputBody+1)} {
			raw, _ := json.Marshal(map[string]string{"body": body})
			h.do(grokAddr, "POST", base+"/answer", string(raw), http.StatusBadRequest, nil)
		}
		h.do(instinctAddr, "POST", base+"/answer", `{"from":"grokbot","body":"spoofed"}`, http.StatusForbidden, nil)
		h.do(grokAddr, "POST", base+"/answer", `{"body":"private answer"}`, http.StatusOK, nil)
		h.do(grokAddr, "POST", base+"/answer", `{"body":"duplicate"}`, http.StatusConflict, nil)
		h.do(museAddr, "POST", base+"/claim", "", http.StatusOK, nil)
	}
	h.do(museAddr, "POST", base+"/reply", question, http.StatusConflict, nil)
	events, err := h.st.AuditEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	questions, answers := 0, 0
	for _, event := range events {
		if strings.Contains(event.Detail, "private") {
			t.Fatalf("body in audit: %+v", event)
		}
		switch event.Event {
		case "needs_input":
			questions++
			if event.Detail != `{"question_bytes":16}` {
				t.Fatalf("question detail: %s", event.Detail)
			}
		case "answered_input":
			answers++
			if event.Detail != `{"answer_bytes":14}` {
				t.Fatalf("answer detail: %s", event.Detail)
			}
		}
	}
	if questions != 3 || answers != 3 {
		t.Fatalf("events: %d questions, %d answers", questions, answers)
	}
	if _, err := h.st.VerifyAudit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationAckHTTP(t *testing.T) {
	h := newHarness(t, Config{})
	req := h.send(grokAddr, "muse", "dinner")
	base := "/v1/requests/" + req.ID
	h.do(museAddr, "POST", base+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", base+"/reply", `{"status":"needs_input","body":"where?"}`, http.StatusOK, nil)
	var in struct {
		Replies []envelope.Result `json:"replies"`
	}
	h.do(grokAddr, "GET", "/v1/poll?replies=take&hold=0", "", http.StatusOK, &in)
	if len(in.Replies) != 1 || in.Replies[0].Reply.Generation == 0 {
		t.Fatalf("poll = %+v", in)
	}
	old := in.Replies[0].Reply.Generation
	h.do(grokAddr, "POST", base+"/answer", `{"body":"here"}`, http.StatusOK, nil)
	h.do(museAddr, "POST", base+"/claim", "", http.StatusOK, nil)
	h.do(museAddr, "POST", base+"/reply", `{"body":"booked"}`, http.StatusOK, nil)
	h.do(grokAddr, "GET", "/v1/poll?replies=take&hold=0", "", http.StatusOK, &in)
	current := in.Replies[0].Reply.Generation
	legacy := h.send(grokAddr, "muse", "legacy request")
	h.answer(museAddr, legacy, "done")
	for _, tc := range []struct {
		generation int64
		want       int
	}{{old, 1}, {current, 0}} {
		raw, err := json.Marshal(map[string]any{"ids": []string{legacy.ID}, "acks": []envelope.ReplyAck{{ID: req.ID, Generation: tc.generation}}})
		if err != nil {
			t.Fatal(err)
		}
		h.do(grokAddr, "POST", "/v1/replies/ack", string(raw), http.StatusNoContent, nil)
		if n := h.srv.UnseenReplies("grokbot"); n != tc.want {
			t.Fatalf("unseen = %d, want %d", n, tc.want)
		}
	}
}
