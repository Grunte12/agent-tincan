package history

// The live sites: everything that differs between chatgpt.com, claude.ai,
// grok.com and gemini.google.com, in one table. Adding a site is one entry here plus its
// reader file; nothing else branches on which site it is.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// webSite is one live site the extension reads and a web agent fronts.
type webSite struct {
	source Source
	// label names the site in replies and logs ("ChatGPT").
	label string
	// host is the site's host, in error text and conversation URLs.
	host string
	// agent is the default name of the site's web agent.
	agent string
	// opPrefix starts each of the site's extension operations
	// ("chatgpt" for chatgpt.list and so on).
	opPrefix string
	// fileTakesConversation: the file operation may carry the
	// conversation id.
	fileTakesConversation bool
	// alwaysGranted: the extension had this site's host access before it
	// reported grants, so an extension whose hello lists none has it.
	alwaysGranted bool
	// reader returns the site's history reader over c.
	reader func(c *Client, now func() time.Time) liveReader
	// nodes reads a detail result into the web agent's view of the
	// current branch.
	nodes func(raw json.RawMessage) ([]webNode, error)
	// stable, when set, is the site's text-stability rule: a reply the
	// site does not mark finished counts as finished once it holds still
	// this long.
	stable *stableRule
	// convPath matches a conversation URL's path on host; its first
	// group is the conversation id.
	convPath *regexp.Regexp
	// noteMissingImages: a reply whose generated images could not all be
	// fetched says so, instead of leaving them out silently.
	noteMissingImages bool
	// planLimitCooldown is how long the site is left alone after an
	// answer ends on the account's rate or plan limit (a webNode marked
	// limited).
	planLimitCooldown time.Duration
	// canonID, when set, turns a conversation id in any of the site's
	// forms into the one form every store (per-asker state, used list,
	// journal, reply footer) holds; ok is false for an id that is not
	// the site's. Without it ids are used as given.
	canonID func(id string) (string, bool)
	// noteLostImages: a reply image that could not be fetched is noted
	// in the reply, since the site's images are captured from the page
	// and may fail where an API download would not.
	noteLostImages bool
	// blockedCooldown, when set, holds every request to the site back
	// this long after it showed an anti-bot check, so the agent does not
	// keep tripping it on the owner's account.
	blockedCooldown time.Duration
}

// canonical returns id in the site's canonical form.
func (s *webSite) canonical(id string) (string, bool) {
	if s.canonID == nil {
		return id, validNativeID(id)
	}
	return s.canonID(id)
}

// stableRule: the same reply text on polls consecutive reads spanning at
// least span.
type stableRule struct {
	polls int
	span  time.Duration
}

// liveReader is a site's history reader: a Reader with the shared live
// read flow under it.
type liveReader interface {
	Reader
	live() *live
}

// webSites is the site table, in the order sites are listed.
var webSites = []*webSite{
	{
		source:                SourceChatGPT,
		label:                 "ChatGPT",
		host:                  "chatgpt.com",
		agent:                 "chatgpt-web",
		opPrefix:              "chatgpt",
		fileTakesConversation: true,
		alwaysGranted:         true,
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewChatGPT(c)
			r.Now = now
			return r
		},
		nodes:    chatgptNodes,
		convPath: convURLPattern,
	},
	{
		source:        SourceClaudeAI,
		label:         "claude.ai",
		host:          "claude.ai",
		agent:         "claude-web",
		opPrefix:      "claudeai",
		alwaysGranted: true,
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewClaudeAI(c)
			r.Now = now
			return r
		},
		nodes:    claudeNodes,
		stable:   &stableRule{polls: DefaultClaudeStablePolls, span: DefaultClaudeStableFor},
		convPath: convURLPattern,
	},
	{
		source:                SourceGrok,
		label:                 "Grok",
		host:                  "grok.com",
		agent:                 "grok-web",
		opPrefix:              "grok",
		fileTakesConversation: true,
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewGrok(c)
			r.Now = now
			return r
		},
		nodes:             grokNodes,
		convPath:          grokConvPath,
		noteMissingImages: true,
		planLimitCooldown: DefaultPlanLimitCooldown,
	},
	{
		source:                SourceGemini,
		label:                 "Gemini",
		host:                  "gemini.google.com",
		agent:                 "gemini-web",
		opPrefix:              "gemini",
		fileTakesConversation: true,
		reader: func(c *Client, now func() time.Time) liveReader {
			r := NewGemini(c)
			r.Now = now
			return r
		},
		nodes:           geminiNodes,
		stable:          &stableRule{polls: DefaultGeminiStablePolls, span: DefaultGeminiStableFor},
		convPath:        geminiURLPattern,
		canonID:         geminiCanonicalID,
		noteLostImages:  true,
		blockedCooldown: DefaultBlockedCooldown,
	},
}

// grokConvPath is grok.com's conversation path, /c/<id>.
var grokConvPath = regexp.MustCompile(`^/c/([A-Za-z0-9][A-Za-z0-9_-]{0,127})/?$`)

// DefaultPlanLimitCooldown is how long a site is left alone after an
// answer ended on the account's rate or plan limit.
const DefaultPlanLimitCooldown = 15 * time.Minute

// siteFor returns src's table entry, or nil when src is not a live site.
func siteFor(src Source) *webSite {
	for _, s := range webSites {
		if s.source == src {
			return s
		}
	}
	return nil
}

// lookupSite is siteFor with an error naming the known sites.
func lookupSite(src Source) (*webSite, error) {
	if s := siteFor(src); s != nil {
		return s, nil
	}
	return nil, fmt.Errorf("unknown site %q (want %s)", src, WebSiteNames())
}

// opKind is what an extension operation does, whatever its site.
type opKind string

const (
	opList   opKind = "list"
	opDetail opKind = "detail"
	opFile   opKind = "file"
	opSend   opKind = "send"
	opClose  opKind = "close"
)

func (k opKind) valid() bool {
	switch k {
	case opList, opDetail, opFile, opSend, opClose:
		return true
	}
	return false
}

// op is the site's operation of kind k.
func (s *webSite) op(k opKind) Op { return Op(s.opPrefix + "." + string(k)) }

// resolve returns op's site and kind; ok is false for an operation no
// site has (extension.reload included).
func (op Op) resolve() (*webSite, opKind, bool) {
	prefix, verb, found := strings.Cut(string(op), ".")
	if !found || !opKind(verb).valid() {
		return nil, "", false
	}
	for _, s := range webSites {
		if s.opPrefix == prefix {
			return s, opKind(verb), true
		}
	}
	return nil, "", false
}

// WebSiteNames lists the --site values, for help and errors
// ("chatgpt, claude-ai, grok or gemini").
func WebSiteNames() string {
	names := make([]string, len(webSites))
	for i, s := range webSites {
		names[i] = string(s.source)
	}
	return joinList(names, "or")
}

// SourceNames lists every history source, for errors ("chatgpt,
// claude-ai, grok, gemini, codex or claude-code").
func SourceNames() string {
	names := make([]string, len(Sources))
	for i, s := range Sources {
		names[i] = string(s)
	}
	return joinList(names, "or")
}

// WebAgentNames lists the sites' default web agent names ("chatgpt-web,
// claude-web, grok-web or gemini-web").
func WebAgentNames() string {
	names := make([]string, len(webSites))
	for i, s := range webSites {
		names[i] = s.agent
	}
	return joinList(names, "or")
}

// LiveSourcesLabel names the live sources in prose ("ChatGPT, claude.ai,
// Grok and Gemini").
func LiveSourcesLabel() string {
	labels := make([]string, len(webSites))
	for i, s := range webSites {
		labels[i] = s.label
	}
	return joinList(labels, "and")
}

// IsLiveSource reports whether src is read live through the extension.
func IsLiveSource(src Source) bool { return siteFor(src) != nil }

// NewLiveReader returns the history reader for live source src over c,
// with now as its clock (the real clock when nil); ok is false when src is
// not a live source.
func NewLiveReader(src Source, c *Client, now func() time.Time) (Reader, bool) {
	s := siteFor(src)
	if s == nil {
		return nil, false
	}
	return s.reader(c, now), true
}

// joinList joins items as prose: "a", "a or b", "a, b or c".
func joinList(items []string, conj string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + conj + " " + items[len(items)-1]
}

func siteSources() []Source {
	out := make([]Source, len(webSites))
	for i, s := range webSites {
		out[i] = s.source
	}
	return out
}
