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

// Dots reads the owner's OpenAI dot's DM (chatgpt.com/dots/<thread>) for
// the dot-web agent. It is not a history source (the site entry is
// webOnly): there is no list, only dots.detail, which the extension
// answers with the DM room's latest messages, oldest first, and the room's
// owner (its creator) and whether the dot is paused:
//
//	{thread, room, owner, paused, items: [{id, at, from, text, attachments}]}
//
// A message whose from is the owner is the owner's (a request this agent
// typed is one); any other from is the dot.
type Dots struct {
	Client *Client
	Now    func() time.Time
}

// NewDots returns a dot DM reader over c.
func NewDots(c *Client) *Dots { return &Dots{Client: c} }

var errDotsNoHistory = errors.New("dots is not a history source")

// Source implements Reader.
func (r *Dots) Source() Source { return SourceDots }

// List implements Reader. A dot's DM is not a history source.
func (r *Dots) List(context.Context, int, Options) (Page, error) { return Page{}, errDotsNoHistory }

// Read implements Reader. A dot's DM is not a history source.
func (r *Dots) Read(context.Context, Query, Options) (Page, error) { return Page{}, errDotsNoHistory }

func (r *Dots) live() *live {
	return &live{
		source:      SourceDots,
		client:      r.Client,
		now:         r.Now,
		detailOp:    OpDotsDetail,
		parseList:   func(json.RawMessage) ([]Conversation, error) { return nil, errDotsNoHistory },
		parseDetail: parseDotsDetail,
		fileArgs:    noFiles,
		canonID:     dotsCanonicalID,
	}
}

// dotsThreadPattern is a dot's thread id as tincan takes it: lowercase
// hex and hyphens (a UUID in practice), loosely.
var dotsThreadPattern = regexp.MustCompile(`^[0-9a-f-]{8,64}$`)

// dotsCanonicalID lowercases a dot thread id and checks its shape.
func dotsCanonicalID(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	return id, dotsThreadPattern.MatchString(id) && validNativeID(id)
}

// dotsConvPath is a dot's DM path on chatgpt.com, /dots/<thread>.
var dotsConvPath = regexp.MustCompile(`^/dots/([0-9A-Fa-f-]{8,64})/?$`)

// dotsNotLoggedIn is the not_logged_in reason for a dot.
func dotsNotLoggedIn(string) string {
	return "not signed in to ChatGPT in Chrome; sign in to chatgpt.com in Chrome, then ask again"
}

// DotFeed is a dot DM read by dots.detail: the thread, its room, the
// owner's account user id, whether the dot is paused, and the latest
// messages, oldest first.
type DotFeed struct {
	Thread   string
	Room     string
	Owner    string
	Paused   bool
	Messages []DotMessage
}

// DotMessage is one message of a dot's DM. Owner is set when the owner
// wrote it; otherwise the dot did.
type DotMessage struct {
	ID          string
	At          time.Time
	From        string
	Text        string
	Attachments int
	Owner       bool
}

type dotsDetail struct {
	Thread string `json:"thread"`
	Room   string `json:"room"`
	Owner  string `json:"owner"`
	Paused bool   `json:"paused"`
	Items  []struct {
		ID          string      `json:"id"`
		At          copilotTime `json:"at"`
		From        string      `json:"from"`
		Text        string      `json:"text"`
		Attachments int         `json:"attachments"`
	} `json:"items"`
}

// ParseDotFeed reads the extension's dots.detail result. A result with no
// owner or no items list, or a message with no id or author, is an error.
func ParseDotFeed(raw json.RawMessage) (DotFeed, error) {
	var d dotsDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return DotFeed{}, err
	}
	if strings.TrimSpace(d.Owner) == "" {
		return DotFeed{}, errors.New("no owner")
	}
	if d.Items == nil {
		return DotFeed{}, errors.New("no items")
	}
	f := DotFeed{Thread: d.Thread, Room: d.Room, Owner: d.Owner, Paused: d.Paused}
	for _, m := range d.Items {
		if m.ID == "" || m.From == "" {
			return DotFeed{}, errors.New("a message without an id or author")
		}
		f.Messages = append(f.Messages, DotMessage{
			ID: m.ID, At: m.At.Time, From: m.From, Text: m.Text,
			Attachments: max(m.Attachments, 0), Owner: m.From == d.Owner,
		})
	}
	return f, nil
}

// dotsNodes reads the DM for the reply wait: each owner message is a user
// node, and each run of dot messages after one is a single reply node
// (dotRun). The feed marks nothing finished, so a reply is finished by the
// site's text-stability rule (DefaultDotStablePolls, DefaultDotStableFor),
// which lets a dot that answers in several messages finish them all.
func dotsNodes(raw json.RawMessage) ([]webNode, error) {
	f, err := ParseDotFeed(raw)
	if err != nil {
		return nil, err
	}
	var out []webNode
	msgs := f.Messages
	for i := 0; i < len(msgs); {
		if msgs[i].Owner {
			out = append(out, webNode{id: msgs[i].ID, user: true, text: msgs[i].Text, at: msgs[i].At})
			i++
			continue
		}
		j := i
		for j < len(msgs) && !msgs[j].Owner {
			j++
		}
		out = append(out, dotRun(msgs[i:j]))
		i = j
	}
	return out, nil
}

// dotRun is one run of the dot's messages as one reply node: its texts
// joined by a blank line (with a note for attachments tincan does not
// carry), identified by the run's first message and dated by its last.
func dotRun(run []DotMessage) webNode {
	var texts []string
	files := 0
	for _, m := range run {
		if t := strings.TrimSpace(m.Text); t != "" {
			texts = append(texts, m.Text)
		}
		files += m.Attachments
	}
	if files > 0 {
		what := "an attachment"
		if files > 1 {
			what = fmt.Sprintf("%d attachments", files)
		}
		texts = append(texts, fmt.Sprintf("(your dot also sent %s; open the DM to see them)", what))
	}
	return webNode{id: run[0].ID, reply: true, text: strings.Join(texts, "\n\n"), at: run[len(run)-1].At}
}

// parseDotsDetail turns the DM into turns: each owner message and the dot
// messages after it, as dotsNodes joins them.
func parseDotsDetail(id string, raw json.RawMessage) (thread, error) {
	nodes, err := dotsNodes(raw)
	if err != nil {
		return thread{}, err
	}
	th := thread{conv: Conversation{Source: SourceDots, ID: id}}
	var cur *turn
	for _, n := range nodes {
		if n.user {
			if cur != nil {
				th.turns = append(th.turns, *cur)
			}
			cur = &turn{prompt: Message{Role: RoleUser, Text: n.text, Time: n.at}, promptID: n.id}
		} else if cur != nil {
			cur.reply = Message{Role: RoleAssistant, Text: n.text, Time: n.at}
		}
		if th.conv.UpdatedAt.Before(n.at) {
			th.conv.UpdatedAt = n.at
		}
	}
	if cur != nil {
		th.turns = append(th.turns, *cur)
	}
	return th, nil
}

// ParseWebThread checks a --thread value for site: the site's canonical
// thread id, and false when the site takes none or the id is malformed.
func ParseWebThread(site Source, id string) (string, bool) {
	s := siteFor(site)
	if s == nil || !s.oneThread {
		return "", false
	}
	return s.canonical(id)
}

// WebSiteTakesThread reports whether site's web agent serves one fixed
// thread, given with --thread.
func WebSiteTakesThread(site Source) bool {
	s := siteFor(site)
	return s != nil && s.oneThread
}
