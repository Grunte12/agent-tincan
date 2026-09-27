package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Grok reads grok.com through the Tincan Chrome extension. The extension
// calls GET /rest/app-chat/conversations (list), then for a conversation
// GET .../response-node?includeThreads=true (the message tree and the
// in-flight responses) and POST .../load-responses (the message bodies),
// and hands the two back as one detail result. Generated images are
// fetched from assets.grok.com by response id and index, so no URL ever
// travels from Go to the extension.
type Grok struct {
	Client *Client
	Window Window
	Now    func() time.Time
	// AgentChats is the web agents' used list; those conversations are
	// left out unless all is asked for (DefaultWebUsedPath from New).
	AgentChats string
}

// NewGrok returns a grok.com reader over c.
func NewGrok(c *Client) *Grok {
	return &Grok{Client: c, AgentChats: DefaultWebUsedPath(SourceGrok)}
}

// Source implements Reader.
func (r *Grok) Source() Source { return SourceGrok }

func (r *Grok) live() *live {
	return &live{
		source:      SourceGrok,
		client:      r.Client,
		window:      r.Window,
		now:         r.Now,
		agentChats:  r.AgentChats,
		listOp:      OpGrokList,
		detailOp:    OpGrokDetail,
		parseList:   parseGrokList,
		parseDetail: parseGrokDetail,
		fileArgs:    grokFileArgs,
	}
}

// List implements Reader.
func (r *Grok) List(ctx context.Context, count int, opts Options) (Page, error) {
	return r.live().List(ctx, count, opts)
}

// Read implements Reader.
func (r *Grok) Read(ctx context.Context, q Query, opts Options) (Page, error) {
	return r.live().Read(ctx, q, opts)
}

type grokList struct {
	Conversations []struct {
		ConversationID string   `json:"conversationId"`
		Title          string   `json:"title"`
		CreateTime     flexTime `json:"createTime"`
		ModifyTime     flexTime `json:"modifyTime"`
	} `json:"conversations"`
}

func parseGrokList(raw json.RawMessage) ([]Conversation, error) {
	var l grokList
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	if l.Conversations == nil {
		return nil, errors.New("no conversations")
	}
	var out []Conversation
	for _, c := range l.Conversations {
		if !validNativeID(c.ConversationID) {
			continue
		}
		up := c.ModifyTime.Time
		if up.IsZero() {
			up = c.CreateTime.Time
		}
		out = append(out, Conversation{Source: SourceGrok, ID: c.ConversationID, Title: c.Title, UpdatedAt: up})
	}
	return out, nil
}

// grokDetail is the extension's grok.detail result: the response-node
// answer's tree and in-flight list, and load-responses' bodies.
type grokDetail struct {
	ConversationID    string            `json:"conversationId"`
	ResponseNodes     []grokNode        `json:"responseNodes"`
	InflightResponses []json.RawMessage `json:"inflightResponses"`
	Responses         []grokResponse    `json:"responses"`
}

type grokNode struct {
	ResponseID       string `json:"responseId"`
	Sender           string `json:"sender"`
	ParentResponseID string `json:"parentResponseId"`
}

type grokResponse struct {
	ResponseID       string   `json:"responseId"`
	Message          string   `json:"message"`
	Query            string   `json:"query"`
	Sender           string   `json:"sender"`
	CreateTime       flexTime `json:"createTime"`
	ParentResponseID string   `json:"parentResponseId"`
	// Partial is true while the response is still being written; a
	// finished response has it false.
	Partial            bool              `json:"partial"`
	GeneratedImageURLs []string          `json:"generatedImageUrls"`
	StreamErrors       []json.RawMessage `json:"streamErrors"`
}

func (r *grokResponse) human() bool { return strings.EqualFold(r.Sender, "human") }

func (r *grokResponse) assistant() bool { return strings.EqualFold(r.Sender, "assistant") }

func (r *grokResponse) text() string {
	if strings.TrimSpace(r.Message) != "" || !r.human() {
		return r.Message
	}
	return r.Query
}

// grokLimitText marks a stream error message that is the account's rate
// or plan limit rather than a failure of this one answer.
var grokLimitText = regexp.MustCompile(`(?i)rate.?limit|too many requests|usage limit|message limit|limit reached|reached (?:your|the) [a-z ]*limit|quota|resource.?exhausted|\b429\b`)

// grokStreamError is one entry of a response's streamErrors: an object
// with a gRPC-style code and a message, or a bare string.
type grokStreamError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

// grokLimitCode is a stream error code meaning a rate or plan limit:
// gRPC RESOURCE_EXHAUSTED (8, by number or name) or HTTP 429.
func grokLimitCode(raw json.RawMessage) bool {
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n == "8" || n == "429"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == "8" || s == "429" || strings.EqualFold(s, "RESOURCE_EXHAUSTED") || grokLimitText.MatchString(s)
	}
	return false
}

// limited reports whether the response ended on a rate or plan limit:
// one of its stream errors has a limit code or a limit message. Only the
// decoded code and message count, never other fields of the error.
func (r *grokResponse) limited() bool {
	for _, raw := range r.StreamErrors {
		var e grokStreamError
		if json.Unmarshal(raw, &e) != nil {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				continue
			}
			e.Message = s
		}
		if grokLimitCode(e.Code) || grokLimitText.MatchString(e.Message) {
			return true
		}
	}
	return false
}

const grokImageScheme = "grok-image://"

// maxGrokImages bounds the image index a pointer may carry.
const maxGrokImages = 32

var grokImageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// images returns placeholders for the response's generated images: the
// pointer names the response and the image's index in generatedImageUrls,
// and the extension looks the URL up again itself.
func (r *grokResponse) images() []Image {
	if !validNativeID(r.ResponseID) || strings.Contains(r.ResponseID, "_") {
		return nil
	}
	var out []Image
	for i, u := range r.GeneratedImageURLs {
		if i >= maxGrokImages || strings.TrimSpace(u) == "" {
			break
		}
		id := fmt.Sprintf("%s_%d", r.ResponseID, i)
		name := path.Base(strings.SplitN(u, "?", 2)[0])
		if !grokImageName.MatchString(name) {
			name = id
		}
		out = append(out, imageRef(grokImageScheme+id, name))
	}
	return out
}

func grokFileArgs(convID, pointer string) (Op, OpArgs, bool) {
	id, ok := strings.CutPrefix(pointer, grokImageScheme)
	if !ok || !validNativeID(id) || !validNativeID(convID) {
		return "", OpArgs{}, false
	}
	rid, n, ok := strings.Cut(id, "_")
	if !ok || rid == "" {
		return "", OpArgs{}, false
	}
	if i, err := strconv.Atoi(n); err != nil || i < 0 || i >= maxGrokImages || strconv.Itoa(i) != n {
		return "", OpArgs{}, false
	}
	return OpGrokFile, OpArgs{FileID: id, ConversationID: convID}, true
}

func decodeGrokDetail(raw json.RawMessage) (grokDetail, error) {
	var d grokDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, err
	}
	if d.ResponseNodes == nil || d.Responses == nil {
		return d, errors.New("no response nodes")
	}
	return d, nil
}

// path returns the responses on the current branch, oldest first: walked
// back through parentResponseId from the newest response in the tree
// (the last node when no response is dated).
func (d *grokDetail) path() []grokResponse {
	if len(d.ResponseNodes) == 0 {
		return nil
	}
	nodes := map[string]grokNode{}
	for _, n := range d.ResponseNodes {
		nodes[n.ResponseID] = n
	}
	bodies := map[string]grokResponse{}
	for _, r := range d.Responses {
		bodies[r.ResponseID] = r
	}
	leaf := d.ResponseNodes[len(d.ResponseNodes)-1].ResponseID
	var newest time.Time
	for _, n := range d.ResponseNodes {
		if r, ok := bodies[n.ResponseID]; ok && r.CreateTime.After(newest) {
			newest, leaf = r.CreateTime.Time, n.ResponseID
		}
	}
	var rev []grokResponse
	seen := map[string]bool{}
	for id := leaf; id != "" && !seen[id]; {
		seen[id] = true
		n, ok := nodes[id]
		if !ok {
			break
		}
		if r, ok := bodies[id]; ok {
			if r.Sender == "" {
				r.Sender = n.Sender
			}
			rev = append(rev, r)
		}
		id = n.ParentResponseID
	}
	out := make([]grokResponse, len(rev))
	for i, r := range rev {
		out[len(rev)-1-i] = r
	}
	return out
}

func parseGrokDetail(id string, raw json.RawMessage) (thread, error) {
	d, err := decodeGrokDetail(raw)
	if err != nil {
		return thread{}, err
	}
	cid := d.ConversationID
	if cid == "" {
		cid = id
	}
	th := thread{conv: Conversation{Source: SourceGrok, ID: cid}}
	var cur *turn
	for _, r := range d.path() {
		switch {
		case r.human():
			if cur != nil {
				th.turns = append(th.turns, *cur)
			}
			cur = &turn{prompt: Message{Role: RoleUser, Text: r.text(), Time: r.CreateTime.Time}, promptID: r.ResponseID}
			if th.conv.UpdatedAt.Before(r.CreateTime.Time) {
				th.conv.UpdatedAt = r.CreateTime.Time
			}
		case r.assistant():
			if cur == nil {
				continue
			}
			if t := r.text(); strings.TrimSpace(t) != "" {
				if cur.reply.Text != "" {
					cur.reply.Text += "\n\n"
				}
				cur.reply.Text += t
				cur.reply.Time = r.CreateTime.Time
			}
			cur.replyImages = append(cur.replyImages, r.images()...)
			if th.conv.UpdatedAt.Before(r.CreateTime.Time) {
				th.conv.UpdatedAt = r.CreateTime.Time
			}
		}
	}
	if cur != nil {
		th.turns = append(th.turns, *cur)
	}
	return th, nil
}

// grokNodes reads the current branch for the reply wait. grok.com marks a
// finished answer explicitly: an assistant response is finished when its
// partial flag is false and the conversation has no in-flight response.
// A response that ended on a rate or plan limit is marked limited.
func grokNodes(raw json.RawMessage) ([]webNode, error) {
	d, err := decodeGrokDetail(raw)
	if err != nil {
		return nil, err
	}
	idle := len(d.InflightResponses) == 0
	var out []webNode
	for _, r := range d.path() {
		n := webNode{id: r.ResponseID, text: r.text(), at: r.CreateTime.Time}
		switch {
		case r.human():
			n.user = true
		case r.assistant():
			n.reply = true
			n.images = len(r.images())
			n.finished = !r.Partial && idle
			n.limited = r.limited()
		default:
			continue
		}
		out = append(out, n)
	}
	return out, nil
}
