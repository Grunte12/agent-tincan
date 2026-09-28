package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Perplexity reads a www.perplexity.ai thread through the Tincan Chrome
// extension for the perplexity-web agent. It is not a history source
// (the site entry is webOnly): there is no list, only the thread read,
// perplexity.detail, which the extension answers with the entries of
// GET /rest/thread/<slug> across its pages, as {slug, entries}.
//
// The entry shape was checked against a signed-in thread read
// (2026-09-28) and is parsed defensively; anything unexpected is
// endpoint_changed. An entry is one question (query_str) and its answer:
// the answer text is the ask_text block's markdown_block (its [n] citation
// markers kept), and the sources are the web_results block's
// web_result_block.web_results, numbered in that list's order so the
// markers line up. Older shapes (a workflow block's answer items, the
// entry's FINAL step text) are read as fallbacks. An entry is finished
// when its status is COMPLETED and its answer markdown's progress is DONE.
type Perplexity struct {
	Client *Client
	Window Window
	Now    func() time.Time
	// AgentChats is the web agent's used list.
	AgentChats string
}

// NewPerplexity returns a www.perplexity.ai thread reader over c.
func NewPerplexity(c *Client) *Perplexity {
	return &Perplexity{Client: c, AgentChats: DefaultWebUsedPath(SourcePerplexity)}
}

// Source implements Reader.
func (r *Perplexity) Source() Source { return SourcePerplexity }

func (r *Perplexity) live() *live {
	return &live{
		source:      SourcePerplexity,
		client:      r.Client,
		window:      r.Window,
		now:         r.Now,
		agentChats:  r.AgentChats,
		detailOp:    OpPerplexityDetail,
		parseList:   func(json.RawMessage) ([]Conversation, error) { return nil, errPerplexityNoList },
		parseDetail: parsePerplexityDetail,
		fileArgs:    func(string, string) (Op, OpArgs, bool) { return "", OpArgs{}, false },
	}
}

var errPerplexityNoList = errors.New("perplexity is not a history source")

// List implements Reader. Perplexity is not a history source.
func (r *Perplexity) List(context.Context, int, Options) (Page, error) {
	return Page{}, errPerplexityNoList
}

// Read implements Reader: only one thread by id (ModeConversation).
func (r *Perplexity) Read(ctx context.Context, q Query, opts Options) (Page, error) {
	if q.Mode != ModeConversation {
		return Page{}, errPerplexityNoList
	}
	return r.live().Read(ctx, q, opts)
}

// pxDetail is the extension's perplexity.detail result.
type pxDetail struct {
	Slug    string            `json:"slug"`
	Entries []json.RawMessage `json:"entries"`
}

// pxEntry is one thread entry: a question and its answer.
type pxEntry struct {
	UUID                 string            `json:"uuid"`
	BackendUUID          string            `json:"backend_uuid"`
	Status               string            `json:"status"`
	QueryStr             string            `json:"query_str"`
	ThreadTitle          string            `json:"thread_title"`
	ThreadURLSlug        string            `json:"thread_url_slug"`
	Text                 json.RawMessage   `json:"text"`
	EntryCreatedDatetime pxTime            `json:"entry_created_datetime"`
	EntryUpdatedDatetime pxTime            `json:"entry_updated_datetime"`
	UpdatedDatetime      pxTime            `json:"updated_datetime"`
	Blocks               []json.RawMessage `json:"blocks"`
}

// pxWebResult is one web result. The flags mark results that are not web
// pages (an image, the owner's attachment or memory, earlier turns); they
// are numbered like the rest but never listed as sources.
type pxWebResult struct {
	Name                  string `json:"name"`
	Title                 string `json:"title"`
	URL                   string `json:"url"`
	IsImage               bool   `json:"is_image"`
	IsAttachment          bool   `json:"is_attachment"`
	IsMemory              bool   `json:"is_memory"`
	IsConversationHistory bool   `json:"is_conversation_history"`
	IsConversationSummary bool   `json:"is_conversation_summary"`
	IsClientContext       bool   `json:"is_client_context"`
}

// source reports whether the result is a web page to list.
func (r pxWebResult) source() bool {
	return !r.IsImage && !r.IsAttachment && !r.IsMemory && !r.IsConversationHistory && !r.IsConversationSummary && !r.IsClientContext
}

type pxWebResults struct {
	WebResults []pxWebResult `json:"web_results"`
}

type pxMarkdown struct {
	Progress string   `json:"progress"`
	Answer   string   `json:"answer"`
	Chunks   []string `json:"chunks"`
}

type pxTextPayload struct {
	Text    string   `json:"text"`
	Chunks  []string `json:"chunks"`
	Variant string   `json:"variant"`
}

type pxWorkflow struct {
	Steps []struct {
		Items []struct {
			Payload struct {
				TextPayload *pxTextPayload `json:"text_payload"`
			} `json:"payload"`
		} `json:"items"`
	} `json:"steps"`
}

// pxBlock is one entry block; only the kinds read here are decoded.
type pxBlock struct {
	IntendedUsage    string        `json:"intended_usage"`
	MarkdownBlock    *pxMarkdown   `json:"markdown_block"`
	WebResultBlock   *pxWebResults `json:"web_result_block"`
	SourcesModeBlock *pxWebResults `json:"sources_mode_block"`
	WorkflowBlock    *pxWorkflow   `json:"workflow_block"`
}

// pxTime reads Perplexity's timestamps: RFC 3339, or an ISO time without
// a zone (taken as UTC). Anything else reads as unknown (zero), never as
// an error: a time is not worth failing the read over.
type pxTime struct{ time.Time }

func (t *pxTime) UnmarshalJSON(b []byte) error {
	t.Time = time.Time{}
	var s string
	if json.Unmarshal(b, &s) != nil || s == "" {
		return nil
	}
	if p, err := time.Parse(time.RFC3339Nano, s); err == nil {
		t.Time = p.UTC()
		return nil
	}
	if p, err := time.Parse("2006-01-02T15:04:05.999999999", s); err == nil {
		t.Time = p.UTC()
	}
	return nil
}

// id is the entry's id: its uuid, else its backend_uuid.
func (e *pxEntry) id() string {
	if e.UUID != "" {
		return e.UUID
	}
	return e.BackendUUID
}

// at is when the question was asked (the entry's creation), else when the
// entry last changed.
func (e *pxEntry) at() time.Time {
	for _, t := range []pxTime{e.EntryCreatedDatetime, e.EntryUpdatedDatetime, e.UpdatedDatetime} {
		if !t.IsZero() {
			return t.Time
		}
	}
	return time.Time{}
}

// finished: the entry's status is COMPLETED and its answer markdown, when
// it has one, is DONE.
func (e *pxEntry) finished() bool {
	if !strings.EqualFold(e.Status, "COMPLETED") {
		return false
	}
	if m := e.markdown(); m != nil && m.Progress != "" {
		return strings.EqualFold(m.Progress, "DONE")
	}
	return true
}

// failed: the entry ended without an answer.
func (e *pxEntry) failed() bool {
	switch strings.ToUpper(e.Status) {
	case "FAILED", "ERROR", "BLOCKED", "CANCELLED", "CANCELED":
		return true
	}
	return false
}

// blocks decodes the entry's blocks, skipping any this reader does not
// know the shape of.
func (e *pxEntry) blocks() []pxBlock {
	var out []pxBlock
	for _, raw := range e.Blocks {
		var b pxBlock
		if json.Unmarshal(raw, &b) == nil {
			out = append(out, b)
		}
	}
	return out
}

func joinText(text string, chunks []string) string {
	if strings.TrimSpace(text) != "" {
		return text
	}
	return strings.Join(chunks, "")
}

// pxFinal is the entry text's FINAL step answer, the fallback shape: the
// entry's text is a JSON list of steps, and the FINAL one's
// content.answer is JSON again, {answer, web_results}.
type pxFinal struct {
	Answer     string        `json:"answer"`
	WebResults []pxWebResult `json:"web_results"`
}

func (e *pxEntry) final() (pxFinal, bool) {
	var s string
	if len(e.Text) == 0 || json.Unmarshal(e.Text, &s) != nil {
		return pxFinal{}, false
	}
	var steps []struct {
		StepType string `json:"step_type"`
		Content  struct {
			Answer string `json:"answer"`
		} `json:"content"`
	}
	if json.Unmarshal([]byte(s), &steps) != nil {
		return pxFinal{}, false
	}
	for _, st := range slices.Backward(steps) {
		if !strings.EqualFold(st.StepType, "FINAL") {
			continue
		}
		var f pxFinal
		if json.Unmarshal([]byte(st.Content.Answer), &f) == nil {
			return f, true
		}
	}
	return pxFinal{}, false
}

// markdown is the entry's answer markdown block: the "ask_text" block's,
// else any ask_text_* block's; nil when there is none.
func (e *pxEntry) markdown() *pxMarkdown {
	blocks := e.blocks()
	for _, exact := range []bool{true, false} {
		for _, b := range blocks {
			u := b.IntendedUsage
			if b.MarkdownBlock == nil || (exact && u != "ask_text") || (!exact && !strings.HasPrefix(u, "ask_text")) {
				continue
			}
			return b.MarkdownBlock
		}
	}
	return nil
}

// answer returns the entry's answer text and its sources.
func (e *pxEntry) answer() (string, []webSource) {
	blocks := e.blocks()
	var text string
	// 1. The ask_text block's markdown.
	if m := e.markdown(); m != nil {
		text = joinText(m.Answer, m.Chunks)
	}
	// 2. The answer items of a workflow block.
	if text == "" {
		var parts []string
		for _, b := range blocks {
			if b.WorkflowBlock == nil {
				continue
			}
			for _, st := range b.WorkflowBlock.Steps {
				for _, it := range st.Items {
					p := it.Payload.TextPayload
					if p == nil || !strings.EqualFold(p.Variant, "answer") {
						continue
					}
					if t := joinText(p.Text, p.Chunks); strings.TrimSpace(t) != "" {
						parts = append(parts, t)
					}
				}
			}
		}
		text = strings.Join(parts, "\n\n")
	}
	var results []pxWebResult
	for _, b := range blocks {
		for _, wr := range []*pxWebResults{b.WebResultBlock, b.SourcesModeBlock} {
			if wr != nil {
				results = append(results, wr.WebResults...)
			}
		}
	}
	// 3. The entry's own text: the FINAL step's answer, or plain text.
	if text == "" || len(results) == 0 {
		if f, ok := e.final(); ok {
			if text == "" {
				text = f.Answer
			}
			if len(results) == 0 {
				results = f.WebResults
			}
		} else if text == "" {
			var s string
			if json.Unmarshal(e.Text, &s) == nil && !json.Valid([]byte(s)) {
				text = s
			}
		}
	}
	// Sources keep their place in the site's list as their number, so
	// the answer's [n] markers name them.
	srcs := make([]webSource, 0, len(results))
	for i, r := range results {
		if !r.source() {
			continue
		}
		title := r.Name
		if strings.TrimSpace(title) == "" {
			title = r.Title
		}
		srcs = append(srcs, webSource{n: i + 1, title: title, url: r.URL})
	}
	return text, srcs
}

// pxEntries decodes a perplexity.detail result. The entries list must be
// there, and every entry must have a valid id, or the shape is unexpected.
func pxEntries(raw json.RawMessage) (pxDetail, []pxEntry, error) {
	var d pxDetail
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, nil, err
	}
	if d.Entries == nil {
		return d, nil, errors.New("no entries")
	}
	out := make([]pxEntry, 0, len(d.Entries))
	for i, r := range d.Entries {
		var e pxEntry
		if err := json.Unmarshal(r, &e); err != nil {
			return d, nil, fmt.Errorf("entry %d: %w", i, err)
		}
		if !validNativeID(e.id()) {
			return d, nil, fmt.Errorf("entry %d: no valid id", i)
		}
		out = append(out, e)
	}
	return d, out, nil
}

// pxAnswer is an entry's answer, checked: a COMPLETED entry with neither
// answer text nor sources in any known place means the shape changed.
func pxAnswer(e *pxEntry) (string, []webSource, error) {
	text, srcs := e.answer()
	if e.finished() && strings.TrimSpace(text) == "" && len(srcs) == 0 {
		return "", nil, fmt.Errorf("entry %s: finished with no answer in any known place", e.id())
	}
	return text, srcs, nil
}

func parsePerplexityDetail(id string, raw json.RawMessage) (thread, error) {
	d, entries, err := pxEntries(raw)
	if err != nil {
		return thread{}, err
	}
	cid := d.Slug
	if !validNativeID(cid) && len(entries) > 0 {
		cid = entries[0].ThreadURLSlug
	}
	if !validNativeID(cid) {
		cid = id
	}
	th := thread{conv: Conversation{Source: SourcePerplexity, ID: cid}}
	for i := range entries {
		e := &entries[i]
		text, srcs, err := pxAnswer(e)
		if err != nil {
			return thread{}, err
		}
		at := e.at()
		if th.conv.Title == "" {
			th.conv.Title = e.ThreadTitle
		}
		if th.conv.UpdatedAt.Before(at) {
			th.conv.UpdatedAt = at
		}
		th.turns = append(th.turns, turn{
			prompt:       Message{Role: RoleUser, Text: e.QueryStr, Time: at},
			promptID:     e.id(),
			reply:        Message{Role: RoleAssistant, Text: text, Time: at},
			replySources: srcs,
		})
	}
	return th, nil
}

// perplexityNodes reads a thread for the reply wait: each entry is a user
// node (its question) and a reply node (its answer). The answer is
// finished when the entry's status is COMPLETED; a failed entry ends the
// turn with no answer.
func perplexityNodes(raw json.RawMessage) ([]webNode, error) {
	_, entries, err := pxEntries(raw)
	if err != nil {
		return nil, err
	}
	var out []webNode
	for i := range entries {
		e := &entries[i]
		text, _, err := pxAnswer(e)
		if err != nil {
			return nil, err
		}
		at := e.at()
		out = append(out,
			webNode{id: e.id(), user: true, text: e.QueryStr, at: at},
			webNode{id: e.id() + "-answer", reply: true, text: text, at: at, finished: e.finished(), endTurn: e.finished() || e.failed()},
		)
	}
	return out, nil
}
