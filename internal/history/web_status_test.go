package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type authChannel struct {
	calls []Op
	code  string
}

func (c *authChannel) Exchange(_ context.Context, req NativeRequest, recv func(NativeResponse) (bool, error)) error {
	c.calls = append(c.calls, req.Op)
	r := NativeResponse{OK: true, Result: json.RawMessage(`{}`)}
	if c.code != "" {
		r.OK = false
		r.Error = &NativeError{Code: c.code}
	}
	_, err := recv(r)
	return err
}

func TestWebAuthenticationObservations(t *testing.T) {
	for _, site := range WebSites {
		t.Run(string(site), func(t *testing.T) {
			var logs bytes.Buffer
			c := &authChannel{code: "not_logged_in"}
			w := &WebAgent{Site: site, Native: &Client{Channel: c}, Log: &logs}
			_, _ = w.authRequest(t.Context(), w.site().op(opDetail), OpArgs{ID: "abc"})
			if w.auth.latest.State != "signed_out" {
				t.Fatal("typed failure was lost")
			}
			w.observeAuth(errors.New("disconnected"))
			w.observeAuth(ErrNotLoggedIn)
			if strings.Count(logs.String(), "signed out of") != 1 {
				t.Fatalf("duplicate transition log: %s", &logs)
			}
			c.code = ""
			w.probeSession(t.Context())
			if w.auth.latest.State != "authenticated" {
				t.Fatal("fresh probe did not clear")
			}
			if err := ValidateOp(w.site().op(opSession), OpArgs{ID: "abc"}); err == nil {
				t.Fatal("probe accepted arguments")
			}
		})
	}
}

func TestOldExtensionProbeDoesNotClear(t *testing.T) {
	c := &authChannel{code: "bad_request"}
	w := &WebAgent{Site: SourceChatGPT, Native: &Client{Channel: c}, Log: &bytes.Buffer{}}
	w.observeAuth(ErrNotLoggedIn)
	w.probeSession(t.Context())
	w.probeSession(t.Context())
	if !w.auth.probeUnsupported || len(c.calls) != 1 || w.auth.latest.State != "signed_out" {
		t.Fatalf("old extension: %+v", w.auth.latest)
	}
}

func TestWebStatusIdleProbe(t *testing.T) {
	c := &authChannel{}
	clock := newFakeClock()
	w := &WebAgent{Site: SourceChatGPT, Native: &Client{Channel: c, Cooldown: &SiteCooldown{Now: clock.Now}}, clock: clock, Log: &bytes.Buffer{}}
	next := w.probeIdle(t.Context())
	if len(c.calls) != 1 || !next.Equal(clock.Now().Add(webProbeInterval)) {
		t.Fatal("startup probe missing")
	}
	w.probeIdle(t.Context())
	if len(c.calls) != 1 {
		t.Fatal("fresh evidence did not replace probe")
	}
	if err := clock.Sleep(t.Context(), webProbeInterval); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.probeIdle(t.Context())
	w.mu.Unlock()
	if len(c.calls) != 1 {
		t.Fatal("probe overlapped active send")
	}
	w.Native.cooldown().Note(w.Site, webProbeInterval)
	w.probeIdle(t.Context())
	if len(c.calls) != 1 {
		t.Fatal("probe ignored cooldown")
	}
	if err := clock.Sleep(t.Context(), webProbeInterval); err != nil {
		t.Fatal(err)
	}
	w.probeIdle(t.Context())
	if len(c.calls) != 2 {
		t.Fatal("idle probe did not resume")
	}
}
