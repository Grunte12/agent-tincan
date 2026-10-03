package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

func doctorRelayCheck(t *testing.T) check {
	t.Helper()
	for _, c := range runDoctor(context.Background(), "", nil).Checks {
		if c.Name == "relay" {
			return c
		}
	}
	t.Fatal("no relay check")
	return check{}
}

func deadRelayURL(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.NotFoundHandler())
	u := ts.URL
	ts.Close()
	return u
}

func TestDoctorUnreachableWithoutKey(t *testing.T) {
	useConfig(t, client.Config{Relay: deadRelayURL(t), Agent: "muse"})
	c := doctorRelayCheck(t)
	if c.Status != "fail" || !strings.Contains(c.Detail, "cannot reach the relay") || !strings.Contains(c.Fix, "no relay key") {
		t.Fatalf("got %+v", c)
	}
	if strings.Contains(c.Detail, "cannot search") || strings.Contains(c.Detail, "listed") {
		t.Fatalf("keyless config should not talk about a netmap search: %+v", c)
	}
}

func TestDoctorUnreachableWithoutNetmap(t *testing.T) {
	t.Cleanup(client.SwapNetmapLookups(
		func(context.Context) ([]string, error) { return nil, errors.New("no localapi") },
		func(context.Context) ([]string, error) { return nil, errors.New("no cli") },
	))
	useConfig(t, client.Config{Relay: deadRelayURL(t), Agent: "muse", RelayKey: "k-real"})
	c := doctorRelayCheck(t)
	if c.Status != "fail" || !strings.Contains(c.Detail, "cannot search the tailnet") || !strings.Contains(c.Fix, "proxy-only") {
		t.Fatalf("got %+v", c)
	}
}

func TestDoctorUnreachableNoneProved(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	t.Cleanup(client.SwapNetmapLookups(
		func(context.Context) ([]string, error) { return []string{"127.0.0.1"}, nil },
		func(context.Context) ([]string, error) { return nil, errors.New("no cli") },
	))
	useConfig(t, client.Config{Relay: "http://127.0.0.2:" + port, Agent: "muse", RelayKey: "k-real"})
	c := doctorRelayCheck(t)
	if c.Status != "fail" || !strings.Contains(c.Detail, "none of 1 tailnet peers listed via localapi") {
		t.Fatalf("got %+v", c)
	}
	if strings.Contains(c.Detail, "cannot search") || strings.Contains(c.Fix, "proxy-only") {
		t.Fatalf("a netmap search that found no proof is not 'cannot search': %+v", c)
	}
}
