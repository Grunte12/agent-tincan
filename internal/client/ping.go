package client

import (
	"context"
	"fmt"
	"log"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// PingResponder claims and replies to automatic reachability checks.
type PingResponder interface {
	Claimer
	Reply(context.Context, string, string, envelope.Status) (envelope.Reply, error)
}

// AnswerPings removes pings from the inbox and answers them without model work.
// The non-ping inbox is always returned. Claim races are benign; other failures
// are logged and returned separately for callers that need retry backoff.
func AnswerPings(ctx context.Context, r PingResponder, in Inbox, surface string) (Inbox, error) {
	rest := in
	rest.Requests = nil
	var first error
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
		if _, err := r.Reply(ctx, req.ID, fmt.Sprintf("pong (answered by %s, tincan %s)", surface, version), envelope.StatusAnswered); err != nil && !IsStatus(err, 409) {
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
	return rest, first
}
