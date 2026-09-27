package client

import (
	"testing"
	"time"
)

// ShortPongRetry makes AnswerPings retry failed pongs without real delays.
func ShortPongRetry(t testing.TB) {
	old := pongRetry
	pongRetry = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { pongRetry = old })
}
