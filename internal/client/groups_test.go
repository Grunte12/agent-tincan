package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestGroupPartialAttachmentsAndSenderScope(t *testing.T) {
	m := attachMesh(t)
	c := m.Client(t, "grokbot")
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	g, err := c.SendGroup(t.Context(), []string{"instinct", "muse", "instinct"}, "question", envelope.KindAsk, "", []string{path})
	if err != nil || len(g.Results) != 2 {
		t.Fatalf("%+v %v", g, err)
	}
	a, b := g.Results[0].Request, g.Results[1].Request
	if a.Group != g.Group || b.Group != g.Group || len(a.Attachments) != 1 || len(b.Attachments) != 1 || a.Attachments[0].ID == b.Attachments[0].ID {
		t.Fatalf("requests: %+v %+v", a, b)
	}
	if _, err := m.Client(t, "instinct").Reply(t.Context(), a.ID, "answer", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	g, err = c.GetGroup(t.Context(), g.Group, 0)
	if err != nil || g.Outcome != "partial" || g.ExitCode() != 2 {
		t.Fatalf("%+v %v", g, err)
	}
	if !strings.Contains(client.FormatGroup(g), "answer") || !strings.Contains(client.FormatGroup(g), b.ID) {
		t.Fatal(client.FormatGroup(g))
	}
	if _, err := m.Client(t, "muse").GetGroup(t.Context(), g.Group, 0); !client.IsStatus(err, 404) {
		t.Fatalf("other sender: %v", err)
	}
	fresh := m.Client(t, "grokbot")
	if got, err := fresh.GetGroup(t.Context(), g.Group, 0); err != nil || len(got.Results) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := m.Client(t, "muse").Reply(t.Context(), b.ID, "second", envelope.StatusAnswered); err != nil {
		t.Fatal(err)
	}
	if got, err := fresh.GetGroup(t.Context(), g.Group, time.Second); err != nil || got.Outcome != "answered" || got.ExitCode() != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestGroupOlderRelay(t *testing.T) {
	requests := map[string]envelope.Request{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/capabilities":
			http.NotFound(w, r)
		case r.URL.Path == "/v1/send":
			var req envelope.Request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			req.ID, req.Group = req.To, ""
			requests[req.ID] = req
			_ = json.NewEncoder(w).Encode(req)
		case strings.HasPrefix(r.URL.Path, "/v1/requests/"):
			req := requests[strings.TrimPrefix(r.URL.Path, "/v1/requests/")]
			_ = json.NewEncoder(w).Encode(client.Result{Request: req, Status: envelope.StatusQueued})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.SendGroup(t.Context(), []string{"a", "b"}, "hello", envelope.KindAsk, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err = c.GetGroup(t.Context(), g.Group, 0)
	if err != nil || len(g.Results) != 2 || g.Outcome != "pending" {
		t.Fatalf("%+v %v", g, err)
	}
	fresh, _ := client.NewRelay(ts.URL, "")
	if _, err := fresh.GetGroup(t.Context(), g.Group, 0); err == nil {
		t.Fatal("expected missing local record")
	}
}

func TestGroupValidationAndSendFailure(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	c := m.Client(t, "grokbot")
	for _, targets := range [][]string{{}, {"instinct", "grokbot"}, {"instinct", ""}, strings.Split("a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q,r,s,t,u,v,w,x,y,z,aa,ab,ac,ad,ae", ",")} {
		if _, err := c.SendGroup(t.Context(), targets, "hello", envelope.KindAsk, "", nil); err == nil {
			t.Fatalf("accepted %v", targets)
		}
	}
	g, err := c.SendGroup(t.Context(), []string{"instinct", "missing"}, "hello", envelope.KindAsk, "", nil)
	if err != nil || g.Results[0].Request.ID == "" || g.Results[1].Status != envelope.StatusFailed {
		t.Fatalf("%+v %v", g, err)
	}
	g, err = c.GetGroup(t.Context(), g.Group, 0)
	if err != nil || len(g.Results) != 2 || g.Outcome != "partial" {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestGroupOutcomes(t *testing.T) {
	for _, tc := range []struct {
		statuses []envelope.Status
		code     int
	}{
		{[]envelope.Status{envelope.StatusAnswered, envelope.StatusAnswered}, 0},
		{[]envelope.Status{envelope.StatusAnswered, envelope.StatusDeclined}, 1},
		{[]envelope.Status{envelope.StatusFailed, envelope.StatusQueued}, 2},
	} {
		var g client.GroupResult
		for _, s := range tc.statuses {
			g.Results = append(g.Results, client.GroupEntry{Result: client.Result{Status: s}})
		}
		if got := g.ExitCode(); got != tc.code {
			t.Fatalf("%v: %d", tc.statuses, got)
		}
	}
}

func TestGroupWaitPollsConcurrently(t *testing.T) {
	var started atomic.Int32
	ready := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if started.Add(1) == 2 {
			close(ready)
		}
		select {
		case <-ready:
		case <-r.Context().Done():
			return
		}
		status := envelope.StatusQueued
		if strings.HasSuffix(r.URL.Path, "/a") {
			status = envelope.StatusAnswered
		}
		_ = json.NewEncoder(w).Encode(client.Result{Request: envelope.Request{ID: r.URL.Path}, Status: status})
	}))
	defer ts.Close()
	c, _ := client.NewRelay(ts.URL, "")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	g, err := c.WaitGroup(ctx, client.GroupResult{Group: "group-test", Results: []client.GroupEntry{
		{Result: client.Result{Request: envelope.Request{ID: "a"}}}, {Result: client.Result{Request: envelope.Request{ID: "b"}}},
	}}, time.Second)
	if err != nil || g.Outcome != "partial" {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestGroupMaximumBodies(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	c := m.Client(t, "grokbot")
	targets := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	peers := make([]*client.Relay, len(targets))
	for i, name := range targets {
		peers[i] = m.JoinOnMachineOf(t, "instinct", name)
	}
	body := strings.Repeat("<", 128<<10)
	g, err := c.SendGroup(t.Context(), targets, body, envelope.KindAsk, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, res := range g.Results {
		if _, err := peers[i].Reply(t.Context(), res.Request.ID, body, envelope.StatusAnswered); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Get(c.Base() + "/v1/groups/" + g.Group)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var members []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(members) != 8 {
		t.Fatalf("status=%d members=%v", resp.StatusCode, members)
	}
	for _, member := range members {
		if len(member) != 2 || member["id"] == "" || member["to"] == "" {
			t.Fatalf("unexpected membership: %v", member)
		}
	}
	if n, err := m.Store.CountUnseenReplies(t.Context(), "grokbot"); err != nil || n != 8 {
		t.Fatalf("unseen after membership lookup=%d err=%v", n, err)
	}
	got, err := m.Client(t, "grokbot").GetGroup(t.Context(), g.Group, 0)
	if err != nil || got.Outcome != "answered" || len(got.Results) != 8 {
		t.Fatalf("outcome=%s count=%d err=%v", got.Outcome, len(got.Results), err)
	}
	for _, res := range got.Results {
		if res.Request.Body != body || res.Reply == nil || res.Reply.Body != body {
			t.Fatalf("missing full bodies for %s", res.Request.ID)
		}
	}
}

func TestGroupPollErrorPreservesResults(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/failed") {
			http.Error(w, "poll unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(client.Result{
			Request: envelope.Request{ID: "success", To: "a"}, Status: envelope.StatusAnswered,
			Reply: &envelope.Reply{From: "a", Status: envelope.StatusAnswered, Body: "saved answer"},
		})
	}))
	defer ts.Close()
	c, err := client.NewRelay(ts.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	g, err := c.WaitGroup(t.Context(), client.GroupResult{Group: "group-errors", Results: []client.GroupEntry{
		{Result: client.Result{Request: envelope.Request{ID: "success", To: "a"}, Status: envelope.StatusQueued}},
		{Result: client.Result{Request: envelope.Request{ID: "failed", To: "b"}, Status: envelope.StatusQueued}},
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if g.Results[1].Status != envelope.StatusQueued || !strings.Contains(g.Results[1].Error, "poll unavailable") || g.Results[0].Reply == nil {
		t.Fatalf("lost partial results: %+v", g)
	}
	encoded, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{client.FormatGroup(g), string(encoded)} {
		for _, want := range []string{"group-errors", "success", "failed", "saved answer", "poll unavailable"} {
			if !strings.Contains(output, want) {
				t.Fatalf("missing %q in %s", want, output)
			}
		}
	}
}
