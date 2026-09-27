package client

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// PingResponder claims and replies to automatic reachability checks.
type PingResponder interface {
	Claimer
	Reply(context.Context, string, string, envelope.Status) (envelope.Reply, error)
}

// PongRetry spaces the retries of a failed pong reply. Tests shorten it.
var PongRetry = []time.Duration{200 * time.Millisecond, time.Second, 3 * time.Second}

// AnswerPings removes pings from the inbox and answers them without model work.
// The non-ping inbox is always returned. Claim races are benign; other failures
// are logged and returned separately for callers that need retry backoff.
// Run the returned retry function only after handing off ordinary work.
func AnswerPings(ctx context.Context, r PingResponder, in Inbox, surface string) (Inbox, func(context.Context), error) {
	rest := in
	rest.Requests = nil
	var first error
	var retries []func(context.Context)
	for _, req := range in.Requests {
		if req.Kind != envelope.KindPing {
			rest.Requests = append(rest.Requests, req)
			continue
		}
		if _, err := r.Claim(ctx, req.ID); err != nil {
			if IsStatus(err, 409) {
				continue
			}
			log.Printf("tincan %s: ping %s claim failed: %v", surface, req.ID, err)
			if first == nil {
				first = err
			}
			continue
		}
		version := Version
		if version == "" {
			version = "dev"
		}
		// The ping is claimed now, so polling will not see it again until the
		// lease runs out; retain failed pongs for retries after ordinary work.
		body := fmt.Sprintf("pong (answered by %s, tincan %s)", surface, version)
		_, err := r.Reply(ctx, req.ID, body, envelope.StatusAnswered)
		if err != nil && !IsStatus(err, 409) {
			retries = append(retries, func(ctx context.Context) {
				for _, delay := range PongRetry {
					select {
					case <-ctx.Done():
						return
					case <-time.After(delay):
					}
					if _, err := r.Reply(ctx, req.ID, body, envelope.StatusAnswered); err == nil || IsStatus(err, 409) {
						return
					}
				}
			})
		}

		if err != nil && !IsStatus(err, 409) {
			log.Printf("tincan %s: ping %s reply failed: %v", surface, req.ID, err)
			if first == nil {
				first = err
			}
		}
	}
	// Ping replies are operational output, never model inbox content.
	rest.Replies = nil
	for _, reply := range in.Replies {
		if reply.Request.Kind != envelope.KindPing {
			rest.Replies = append(rest.Replies, reply)
		}
	}
	return rest, func(ctx context.Context) {
		for _, retry := range retries {
			retry(ctx)
		}
	}, first
}

// RetryPongs runs deferred pong retries within a bounded background context.
func RetryPongs(ctx context.Context, retry func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	retry(ctx)
}
