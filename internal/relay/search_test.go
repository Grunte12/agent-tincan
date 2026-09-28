package relay

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestSearchScopeAndAudit(t *testing.T) {
	h := newHarness(t, Config{})
	var root, child envelope.Request
	h.do(grokAddr, "POST", "/v1/send", `{"to":"instinct","body":"dinner plans"}`, 201, &root)
	var err error
	child, err = h.st.Enqueue(t.Context(), envelope.Request{From: "instinct", To: "muse", Body: "restaurant booking", Kind: envelope.KindAsk, ParentID: root.ID, TraceID: root.ID, Hop: 2, Chain: []string{"grokbot", "instinct"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h.do(museAddr, "POST", "/v1/requests/"+child.ID+"/reply", `{"body":"restaurant confirmed"}`, 200, nil)
	h.do(museAddr, "POST", "/v1/send", `{"to":"instinct","body":"restaurant private"}`, 201, nil)
	var out struct {
		Results []struct {
			RequestID string `json:"request_id"`
			TraceID   string `json:"trace_id"`
			Snippet   string `json:"snippet"`
		} `json:"results"`
	}
	h.do(grokAddr, "GET", "/v1/search?q=restaurant", "", 200, &out)
	if len(out.Results) != 1 || out.Results[0].RequestID != child.ID || out.Results[0].TraceID != root.ID || !strings.Contains(out.Results[0].Snippet, "restaurant") {
		t.Fatalf("results = %+v", out)
	}
	h.do(macAddr, "GET", "/v1/search?q=restaurant", "", 200, &out)
	if len(out.Results) != 2 {
		t.Fatalf("admin results = %+v", out)
	}
	h.do(strangerAddr, "GET", "/v1/search?q=restaurant", "", http.StatusForbidden, nil)
	for _, path := range []string{"/v1/search", "/v1/search?q=x&limit=0", "/v1/search?q=x&limit=51", "/v1/search?q=x&limit=bad"} {
		h.do(grokAddr, "GET", path, "", 400, nil)
	}
	rows, err := h.st.DB().Query(`SELECT detail FROM audit WHERE event = 'search' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(details) != 2 || details[0] != `{"count":1}` || details[1] != `{"count":2}` {
		t.Fatalf("audit = %v", details)
	}
}
