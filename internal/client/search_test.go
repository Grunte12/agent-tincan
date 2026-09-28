package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

func TestSearchClient(t *testing.T) {
	m := testrelay.New(t, relay.Config{})
	r := m.Client(t, "grokbot")
	req, err := r.Send(t.Context(), "muse", `restaurant & sushi "booked"`, envelope.KindAsk, "", false)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := r.Search(t.Context(), `restaurant & sushi "booked"`, 0)
	if err != nil || len(hits) != 1 || hits[0].RequestID != req.ID {
		t.Fatalf("escaped query = %+v, %v", hits, err)
	}
	caps, err := r.Capabilities(t.Context())
	if err != nil || !caps.Search {
		t.Fatalf("capabilities = %+v, %v", caps, err)
	}
}

func TestSearchOlderRelay(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	r, err := client.NewRelayFor(client.Config{Relay: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Search(t.Context(), "restaurant", 20); err == nil || !strings.Contains(err.Error(), "upgrade the relay") {
		t.Fatalf("old relay = %v", err)
	}
}
