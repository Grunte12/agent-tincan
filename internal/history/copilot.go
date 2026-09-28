package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Copilot reads the owner's personal Microsoft Copilot (copilot.com, where
// copilot.microsoft.com now sends a personal account) through the Tincan
// Chrome extension. copilot.detail is the conversation page's own JSON
// (GET /chat/conversation/<id> asking for application/json, with the
// owner's cookies and no token), cut down in the extension to the ids,
// authors, texts, times and source links read here. copilot.list has no
// JSON to read: the extension reads the chat links in copilot.com's
// sidebar, in a background tab of its own, so the list carries no times
// and is in the sidebar's order (newest first).
type Copilot struct {
	Client *Client
	Window Window
	Now    func() time.Time
	// AgentChats is the web agents' used list; those conversations are
	// left out unless all is asked for (DefaultWebUsedPath from New).
	AgentChats string
}

// NewCopilot returns a copilot.com reader over c.
func NewCopilot(c *Client) *Copilot {
	return &Copilot{Client: c, AgentChats: DefaultWebUsedPath(SourceCopilot)}
}

// Source implements Reader.
func (r *Copilot) Source() Source { return SourceCopilot }

func (r *Copilot) live() *live {
	return &live{
		source:      SourceCopilot,
		client:      r.Client,
		window:      r.Window,
		now:         r.Now,
		agentChats:  r.AgentChats,
		listOp:      OpCopilotList,
		detailOp:    OpCopilotDetail,
		parseList:   parseCopilotList,
		listMore:    copilotListMore,
		parseDetail: parseCopilotDetail,
		fileArgs:    noFiles,
		canonID:     copilotCanonicalID,
		undatedList: true,
	}
}

// List implements Reader.
func (r *Copilot) List(ctx context.Context, count int, opts Options) (Page, error) {
	return r.live().List(ctx, count, opts)
}

// Read implements Reader.
func (r *Copilot) Read(ctx context.Context, q Query, opts Options) (Page, error) {
	return r.live().Read(ctx, q, opts)
}

// noFiles is the file mapping of a site with no file operation: Copilot
// replies carry no images the extension fetches.
func noFiles(string, string) (Op, OpArgs, bool) { return "", OpArgs{}, false }

// copilotIDPattern is a Copilot conversation id: a UUID, lowercase.
var copilotIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// copilotCanonicalID lowercases a Copilot conversation id and checks it
// is a UUID.
func copilotCanonicalID(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	return id, copilotIDPattern.MatchString(id)
}

// copilotConvPath is copilot.com's conversation path,
// /chat/conversation/<uuid>.
var copilotConvPath = regexp.MustCompile(`^/chat/conversation/([0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12})/?$`)

type copilotList struct {
	// More is set by the extension when it stopped reading the sidebar
	// while it was still loading more chats.
	More          bool `json:"more"`
	Conversations []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"conversations"`
}

func copilotListMore(raw json.RawMessage) bool {
	var l copilotList
	return json.Unmarshal(raw, &l) == nil && l.More
}

// parseCopilotList keeps the sidebar's order; the conversations carry no
// time (live.undatedList).
func parseCopilotList(raw json.RawMessage) ([]Conversation, error) {
	var l copilotList
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	if l.Conversations == nil {
		return nil, errors.New("no conversations")
	}
	var out []Conversation
	for _, c := range l.Conversations {
		id, ok := copilotCanonicalID(c.ID)
		if !ok {
			continue
		}
		out = append(out, Conversation{Source: SourceCopilot, ID: id, Title: c.Title})
	}
	return out, nil
}

// copilotTime reads Copilot's timestamps: RFC 3339, or the same without a
// zone (UTC). Anything else is no time rather than an error, so an odd
// timestamp never fails a read.
type copilotTime struct{ time.Time }

func (t *copilotTime) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
		if p, err := time.Parse(layout, s); err == nil {
			t.Time = p.UTC()
			return nil
		}
	}
	return nil
}

// copilotDetail is the extension's copilot.detail result.
type copilotDetail struct {
	ConversationID string           `json:"conversationId"`
	Title          string           `json:"title"`
	CreatedAt      copilotTime      `json:"createdAt"`
	UpdatedAt      copilotTime      `json:"updatedAt"`
	Messages       []copilotMessage `json:"messages"`
}

type copilotMessage struct {
	ID        string      `json:"id"`
	Author    string      `json:"author"`
	Text      string      `json:"text"`
	CreatedAt copilotTime `json:"createdAt"`
	Sources   []webSource `json:"sources"`
}

func decodeCopilotDetail(raw json.RawMessage) (copilotDetail, error) {
	var d copilotDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, err
	}
	if d.Messages == nil {
		return d, errors.New("no messages")
	}
	return d, nil
}

// id is message i's id: the site's message id, or its position when the
// site gave none, so the reply wait and the reply read agree on it.
func (m copilotMessage) id(i int) string {
	if validNativeID(m.ID) {
		return m.ID
	}
	return fmt.Sprintf("copilot-message-%d", i)
}

func parseCopilotDetail(id string, raw json.RawMessage) (thread, error) {
	d, err := decodeCopilotDetail(raw)
	if err != nil {
		return thread{}, err
	}
	cid, ok := copilotCanonicalID(d.ConversationID)
	if !ok {
		cid = id
	}
	th := thread{conv: Conversation{Source: SourceCopilot, ID: cid, Title: d.Title, UpdatedAt: d.UpdatedAt.Time}}
	var cur *turn
	for i, m := range d.Messages {
		at := m.CreatedAt.Time
		switch m.Author {
		case "user":
			if cur != nil {
				th.turns = append(th.turns, *cur)
			}
			cur = &turn{prompt: Message{Role: RoleUser, Text: m.Text, Time: at}, promptID: m.id(i)}
		case "bot":
			if cur == nil {
				continue
			}
			if strings.TrimSpace(m.Text) != "" {
				if cur.reply.Text != "" {
					cur.reply.Text += "\n\n"
				}
				cur.reply.Text += m.Text
				cur.reply.Time = at
			}
			cur.sources = append(cur.sources, m.Sources...)
		default:
			continue
		}
		if th.conv.UpdatedAt.Before(at) {
			th.conv.UpdatedAt = at
		}
	}
	if cur != nil {
		th.turns = append(th.turns, *cur)
	}
	return th, nil
}

// copilotNodes reads the conversation for the reply wait. The page JSON
// marks no answer finished, so a Copilot reply is finished by the site's
// text-stability rule (DefaultCopilotStablePolls, DefaultCopilotStableFor).
func copilotNodes(raw json.RawMessage) ([]webNode, error) {
	d, err := decodeCopilotDetail(raw)
	if err != nil {
		return nil, err
	}
	var out []webNode
	for i, m := range d.Messages {
		n := webNode{id: m.id(i), text: m.Text, at: m.CreatedAt.Time}
		switch m.Author {
		case "user":
			n.user = true
		case "bot":
			n.reply = true
		default:
			continue
		}
		out = append(out, n)
	}
	return out, nil
}
