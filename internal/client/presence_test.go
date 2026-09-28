package client

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// KeepPresence refreshes with peeks that hold nothing and take nothing,
// never a poll, and stops for good when stop returns or the cap passes.
// The relay is an in-memory transport and time is synctest's, so every
// refresh is counted the moment it is made and the counts are exact.
func TestKeepPresencePeeksUntilStoppedOrCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var calls []string
		r, err := NewRelay("http://relay.test", "")
		if err != nil {
			t.Fatal(err)
		}
		r.polls = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			calls = append(calls, req.Method+" "+req.URL.RequestURI())
			mu.Unlock()
			return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		})}
		count := func() int { mu.Lock(); defer mu.Unlock(); return len(calls) }

		// Refreshes at 5ms, 10ms, ... 60ms.
		stop := r.KeepPresence(t.Context(), Presence{Every: 5 * time.Millisecond})
		time.Sleep(62 * time.Millisecond)
		stop()
		if n := count(); n != 12 {
			t.Fatalf("%d presence refreshes in 62ms, want 12", n)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if n := count(); n != 12 {
			t.Fatalf("presence refreshes went on after stop returned: %d", n)
		}
		mu.Lock()
		for _, c := range calls {
			if c != "GET /v1/poll?peek=1&hold=0&replies=keep" {
				t.Errorf("presence made %q, want only a peek with no hold", c)
			}
		}
		mu.Unlock()

		// Refreshes at 5ms, ... 30ms, then the cap at 32ms.
		var logMu sync.Mutex
		var logged []string
		stop = r.KeepPresence(t.Context(), Presence{Every: 5 * time.Millisecond, Cap: 32 * time.Millisecond, Logf: func(f string, a ...any) {
			logMu.Lock()
			logged = append(logged, fmt.Sprintf(f, a...))
			logMu.Unlock()
		}})
		defer stop()
		time.Sleep(time.Second)
		synctest.Wait()
		if n := count() - 12; n != 6 {
			t.Fatalf("%d presence refreshes before a 32ms cap, want 6", n)
		}
		logMu.Lock()
		defer logMu.Unlock()
		if len(logged) != 1 || !strings.Contains(logged[0], "stopped keeping this agent online after 32ms") {
			t.Fatalf("cap log = %q", logged)
		}
	})
}
