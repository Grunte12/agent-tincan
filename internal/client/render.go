package client

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// FormatRequest renders an incoming request for the receiving model. Requests
// come from joined agents, which are trusted teammates, so the framing tells
// the agent to handle them as it would a request from the owner, while keeping the
// sender and chain visible.
func FormatRequest(req envelope.Request) string {
	if req.Kind == envelope.KindPing {
		return ""
	}
	var b strings.Builder
	if req.Urgent {
		b.WriteString("URGENT ")
	}
	fmt.Fprintf(&b, "Request %s from %s (your teammate), via Agent Tincan.\n", req.ID, req.From)
	if len(req.Chain) > 1 {
		fmt.Fprintf(&b, "Chain so far: %s (hop %d).\n", strings.Join(req.Chain, " -> "), req.Hop)
	}
	b.WriteString("Handle it as you would a request from the owner. When you are done, reply with `tincan reply " + req.ID + " \"...\"` (or the reply tool).\n")
	b.WriteString("---\n")
	b.WriteString(req.Body)
	b.WriteString("\n---\n")
	b.WriteString(FormatAttachments(req.Attachments))
	if req.Resumed {
		b.WriteString("Resumed with clarification from the asker.\n")
	}
	b.WriteString(formatExchanges(req.Exchanges))
	return b.String()
}

// FormatResult renders the state of a request this agent sent.
func FormatResult(r Result) string {
	switch {
	case r.Status == envelope.StatusNeedsInput:
		return FormatReply(r)
	case r.Reply != nil:
		return fmt.Sprintf("%s replied (%s):\n%s\n", r.Reply.From, r.Reply.Status, r.Reply.Body) + FormatAttachments(r.Reply.Attachments) + formatExchanges(r.Exchanges)
	case r.Status == envelope.StatusClaimed && r.Progress != nil:
		return fmt.Sprintf("Request %s: %s. Check later with get_reply or `tincan get %s`.\n", r.Request.ID, FormatProgress(r.Progress), r.Request.ID)
	case r.Done():
		return fmt.Sprintf("Request %s to %s ended: %s\n", r.Request.ID, r.Request.To, r.Status)
	default:
		return scheduleHint(r.Request.To, r.Target) + fmt.Sprintf("No reply yet from %s. Request id %s (status %s). Check later with get_reply or `tincan get %s`.\n",
			r.Request.To, r.Request.ID, r.Status, r.Request.ID)
	}
}

// scheduleHint tells a sender how often a schedule target checks its inbox
// and when to expect a reply, from the facts the relay sent with the
// request. It is empty without them: an old relay, or a target that is not
// on a schedule.
func scheduleHint(to string, t *envelope.Target) string {
	if t == nil || t.CheckEverySeconds <= 0 {
		return ""
	}
	hint := fmt.Sprintf("%s checks its inbox every %s", to, compactSeconds(t.CheckEverySeconds))
	if t.ExpectReplySeconds > 0 {
		hint += fmt.Sprintf("; expect a reply within about %s", compactSeconds(t.ExpectReplySeconds))
	}
	hint += "."
	if t.Overdue {
		hint += fmt.Sprintf(" %s has missed its recent checks, so its schedule may have stopped; the owner may need to restart it.", to)
	}
	return hint + "\n"
}

// WakeLabel is the agent's wake method, with its check interval for an
// agent on a schedule: "schedule (every 5m)".
func (a AgentInfo) WakeLabel() string {
	if a.CheckEverySeconds <= 0 {
		return a.Wake
	}
	return fmt.Sprintf("%s (every %s)", a.Wake, compactSeconds(a.CheckEverySeconds))
}

// OverdueNote is "overdue: last check 25m ago" (or "overdue: never
// checked") for a schedule agent the relay marks overdue, measured from its
// last inbox check, and empty otherwise.
func (a AgentInfo) OverdueNote(now time.Time) string {
	if !a.Overdue {
		return ""
	}
	if a.LastPoll.IsZero() {
		return "overdue: never checked"
	}
	return "overdue: last check " + ageAgo(now.Sub(a.LastPoll))
}

// ageAgo renders d like LastSeen does: "just now", "12m ago", "3h ago",
// "2d ago".
func ageAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

// compactSeconds renders a whole number of seconds without zero units:
// 300 is "5m", 90 is "1m30s", 5400 is "1h30m".
func compactSeconds(n int) string {
	s := (time.Duration(n) * time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// FormatProgress renders the latest note with its author and age, on one
// line: a note with newlines must not pass for another request or event in
// a trace.
func FormatProgress(p *envelope.Progress) string {
	note := strings.Join(strings.Fields(p.Note), " ")
	return fmt.Sprintf("claimed by %s, %s ago: %s", p.By, max(time.Duration(0), time.Since(p.At)).Truncate(time.Second), note)
}

// RepliesHeading introduces replies to the agent's own requests in an inbox.
const RepliesHeading = "Replies to your requests:\n"

// FormatReply renders a reply to a request this agent sent, with what it
// asked, since a fresh session may not remember.
func FormatReply(r Result) string {
	if r.Request.Kind == envelope.KindPing {
		return ""
	}
	var b strings.Builder
	if r.Status == envelope.StatusNeedsInput && r.Reply != nil {
		fmt.Fprintf(&b, "%s needs more information for your request %s: %s\nYou asked: %s\nAnswer with answer (or `tincan answer %s \"...\"`).\n", r.Request.To, r.Request.ID, r.Reply.Body, truncate(r.Request.Body, 300), r.Request.ID)
		b.WriteString(formatParent(r.Parent))
		b.WriteString(formatExchanges(r.Exchanges))
		return b.String()
	}
	from, status, body := r.Request.To, r.Status, ""
	var atts []envelope.Attachment
	if r.Reply != nil {
		from, status, body, atts = r.Reply.From, r.Reply.Status, r.Reply.Body, r.Reply.Attachments
	}
	fmt.Fprintf(&b, "Request %s to %s: %s replied (%s).\n", r.Request.ID, r.Request.To, from, status)
	fmt.Fprintf(&b, "You asked: %s\n", truncate(r.Request.Body, 300))
	b.WriteString(formatParent(r.Parent))
	b.WriteString("Finish the work that was waiting on this reply.\n")
	b.WriteString("---\n")
	b.WriteString(body)
	b.WriteString("\n---\n")
	b.WriteString(FormatAttachments(atts))
	b.WriteString(formatExchanges(r.Exchanges))
	return b.String()
}

func formatExchanges(exchanges []envelope.Exchange) string {
	var b strings.Builder
	for i, ex := range exchanges {
		fmt.Fprintf(&b, "Clarification %d:\nQuestion: %s\n", i+1, ex.Question)
		if ex.Answer != "" {
			fmt.Fprintf(&b, "Answer: %s\n", ex.Answer)
		}
	}
	return b.String()
}

// FormatAttachments lists a message's attachments by id, with the sender's
// display name, type and size, and how to fetch one. It is empty when there
// are none.
func FormatAttachments(atts []envelope.Attachment) string {
	if len(atts) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Attachments (%d), fetch one with `tincan attachment get <id>`:\n", len(atts))
	for _, a := range atts {
		fmt.Fprintf(&b, "  %s %q (%s, %d bytes)\n", a.ID, a.Name, a.MIME, a.Size)
	}
	return b.String()
}

// parentOpen reports whether a parent request still expects a reply.
func parentOpen(s envelope.Status) bool {
	return s == envelope.StatusQueued || s == envelope.StatusDelivered || s == envelope.StatusClaimed
}

// Claimer claims a delivered request for this agent.
type Claimer interface {
	Claim(ctx context.Context, id string) (envelope.Request, error)
}

// FormatInbox renders what a poll picked up: replies to this agent's own
// requests first, under RepliesHeading, then the new requests, each claimed
// through c so no one else handles it. The caller acknowledges the replies
// (AckReplies) once the text has reached its agent.
func FormatInbox(ctx context.Context, c Claimer, in Inbox) string {
	var b strings.Builder
	if in.Empty() {
		b.WriteString("No requests waiting.\n")
	}
	if len(in.Replies) > 0 {
		b.WriteString(RepliesHeading)
		for _, r := range in.Replies {
			b.WriteString(FormatReply(r))
		}
		if in.RepliesRemaining > 0 {
			fmt.Fprintf(&b, "%d more %s waiting. Run check_inbox (or `tincan inbox`) again to read %s.\n",
				in.RepliesRemaining, plural(in.RepliesRemaining, "reply is", "replies are"), plural(in.RepliesRemaining, "it", "them"))
		}
		if len(in.Requests) > 0 {
			b.WriteString("\nRequests from teammates:\n")
		}
	}
	for _, req := range in.Requests {
		if req.Kind == envelope.KindPing {
			continue
		}
		if _, err := c.Claim(ctx, req.ID); err != nil {
			fmt.Fprintf(&b, "(could not claim %s: %v)\n", req.ID, err)
			continue
		}
		b.WriteString(FormatRequest(req))
	}
	b.WriteString(UpgradeNotice(in.UpgradeAvailable))
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// truncate shortens s to at most n bytes on a rune boundary, on one line.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func formatParent(p *envelope.Parent) string {
	var b strings.Builder
	if p != nil {
		fmt.Fprintf(&b, "This answers the question you asked while handling request %s from %s: %s.", p.ID, p.From, truncate(p.Body, 300))
		if p.Status == envelope.StatusNeedsInput {
			fmt.Fprintf(&b, " That request is waiting for its sender to answer your clarifying question, so it cannot take a reply yet. Carry on once the answer arrives and the request comes back to you.\n")
		} else if parentOpen(p.Status) {
			fmt.Fprintf(&b, " That request is still open (status %s). When you have what you need, reply to it with `tincan reply %s \"...\"` (or the reply tool).\n", p.Status, p.ID)
		} else {
			fmt.Fprintf(&b, " That request is already closed (status %s), so there is nothing left to reply to.\n", p.Status)
		}
	}
	return b.String()
}

// FormatGroup labels every result and supplies the shared follow-up id.
func FormatGroup(g GroupResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Group %s (%s). Check with get_reply or tincan get %s.\n", g.Group, g.Outcome, g.Group)
	for _, r := range g.Results {
		fmt.Fprintf(&b, "%s (%s), request %s:\n%s", r.Request.To, r.Status, r.Request.ID, FormatResult(r.Result))
		if r.Error != "" {
			fmt.Fprintf(&b, "Poll error: %s\n", r.Error)
		}
	}
	return b.String()
}
