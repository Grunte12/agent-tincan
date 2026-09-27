package client

import (
	"context"
	"fmt"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// PingResponder claims and replies to automatic reachability checks.
type PingResponder interface {
	Claimer
	Reply(context.Context, string, string, envelope.Status) (envelope.Reply, error)
}

// AnswerPings removes pings from the inbox and answers them without model work.
// Failed claims or replies are returned to the caller for retry/reporting.
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
			if first == nil {
				first = err
			}
			continue
		}
		version := Version
		if version == "" {
			version = "dev"
		}
		if _, err := r.Reply(ctx, req.ID, fmt.Sprintf("pong (answered by %s, tincan %s)", surface, version), envelope.StatusAnswered); err != nil && first == nil {
			first = err
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
