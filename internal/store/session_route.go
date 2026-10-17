package store

import (
	"context"
	"fmt"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func sessionMatches(ctx context.Context, bound string) bool {
	session := envelope.Session(ctx)
	return session == "" || bound == "" || bound == session
}

func recipientSessionMatches(req envelope.Request, session string) bool {
	return targetSessionMatches(req, session) &&
		(req.ClaimedSession == "" || req.ClaimedSession == session)
}

func targetSessionMatches(req envelope.Request, session string) bool {
	return session == "" || req.ToSession == "" || req.ToSession == session
}

// column is always a source constant. Encoding the scope as a SQLite hex
// literal keeps this clause reusable without adding positional arguments.
func sessionClause(ctx context.Context, column string) string {
	return fmt.Sprintf("(%s = '' OR %s = CAST(X'%x' AS TEXT))", column, column, []byte(envelope.Session(ctx)))
}
