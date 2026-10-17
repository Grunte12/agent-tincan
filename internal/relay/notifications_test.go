package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestNotificationsRejectScopedCaller(t *testing.T) {
	h := newHarness(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "/v1/notifications", nil)
	req.RemoteAddr = museAddr
	req.Header.Set(client.SessionHeader, "00000000-0000-4000-8000-000000000001")
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("scoped notification status = %d, body %s", rec.Code, rec.Body.String())
	}
}

func TestNotificationsLongPollWakesWithMetadataOnly(t *testing.T) {
	h := newHarness(t, Config{PollHold: 2 * time.Second})
	const session = "00000000-0000-4000-8000-000000000001"
	h.catalogDo(museAddr, "muse", session, "POST", "/v1/session-discovery",
		`{"host":"muse","project":"/workspace/notifications"}`, http.StatusOK, nil)
	var rec *httptest.ResponseRecorder
	var wg sync.WaitGroup
	wg.Go(func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/notifications?hold=2", nil)
		req.RemoteAddr = museAddr
		rec = httptest.NewRecorder()
		h.h.ServeHTTP(rec, req)
	})
	time.Sleep(50 * time.Millisecond)
	var queued envelope.Request
	h.do(grokAddr, http.MethodPost, "/v1/send", `{"to":"muse","session_id":"`+session+`","body":"private queued content"}`, http.StatusCreated, &queued)
	wg.Wait()
	if rec == nil || rec.Code != http.StatusOK {
		t.Fatalf("notification response = %#v", rec)
	}
	var got struct {
		Waiting       int                   `json:"waiting"`
		Queued        int                   `json:"queued"`
		Notifications []client.Notification `json:"notifications"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Waiting != 1 || got.Queued != 1 || len(got.Notifications) != 1 {
		t.Fatalf("notification counts = %+v", got)
	}
	n := got.Notifications[0]
	if n.ID != queued.ID || n.SessionID != session || n.Kind != "request" || n.Generation != 0 {
		t.Fatalf("notification = %+v", n)
	}
	if strings.Contains(rec.Body.String(), "private queued content") {
		t.Fatalf("response leaked request body: %s", rec.Body.String())
	}
	_, status, err := h.st.Request(t.Context(), queued.ID)
	if err != nil || status != envelope.StatusQueued {
		t.Fatalf("request after observation = %s, %v", status, err)
	}
}
