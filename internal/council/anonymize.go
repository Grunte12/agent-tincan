package council

import (
	"regexp"
	"slices"
	"strings"
)

// Web agents end each reply with a tail after the answer text: an
// optional truncation notice, a "Sources:" list, notes, the
// "<Site> conversation: <id>" footer, and image lines. A dot's reply
// ends in "<label>'s DM: <id>" instead ("your dot's DM", sites.go) and
// may carry dotRun's attachment and delegation notes (history/dots.go).
// The footer and notes name the site, so reviewers get the answer text
// only.
var (
	webFooter = regexp.MustCompile(`\n\n[^\n]{1,40} conversation: \S+(?:\n[^\n]*)*$`)
	// dmFooter is tried only when webFooter does not match, so a site
	// reply that mentions someone's DM strips exactly as before.
	dmFooter = regexp.MustCompile(`\n\n[^\n]{1,40}'s DM: \S+(?:\n[^\n]*)*$`)
	webNotes = []*regexp.Regexp{
		regexp.MustCompile(`^Sources:\n`),
		regexp.MustCompile(`^\(reply truncated: showing \d+ of \d+ bytes\)$`),
		regexp.MustCompile(`^Your previous .{1,40} conversation \(id [^)]*\) .*new chat\.$`),
		regexp.MustCompile(`^.{1,40}'s reply had \d+ images? that could not be attached; open the conversation to see (?:it|them)\.$`),
		regexp.MustCompile(`^\(your dot also sent (?:an attachment|\d+ attachments); open the DM to see them\)$`),
		regexp.MustCompile(`^\(your dot asked \S+ for help; tincan will pass \S+'s answer to the dot, and the dot's final answer will be in its DM(?:: \S+)?\)$`),
	}
)

// stripWebTail returns a web agent's reply without its tail. A reply
// without the footer is returned as is.
func stripWebTail(body string) string {
	// The real footer is the last one: an answer may quote a line that
	// looks like either kind earlier on.
	loc := lastFooter(webFooter, body)
	dot := false
	if dm := lastFooter(dmFooter, body); dm != nil && (loc == nil || dm[0] > loc[0]) {
		loc, dot = dm, true
	}
	if loc == nil {
		return body
	}
	paras := strings.Split(body[:loc[0]], "\n\n")
	for len(paras) > 1 && isWebNote(paras[len(paras)-1]) {
		paras = paras[:len(paras)-1]
	}
	if dot {
		// A dot that delegated wrote an @tincan ask message; it would
		// name the dot in a blind review.
		paras = slices.DeleteFunc(paras, func(p string) bool {
			return len(paras) > 1 && dotAskLine.MatchString(p)
		})
	}
	return strings.TrimSpace(strings.Join(paras, "\n\n"))
}

// dotAskLine is a dot's outbound ask paragraph (see internal/history
// dots_out.go parseDotAsk).
var dotAskLine = regexp.MustCompile(`(?i)^[*_\x60\s]*@tincan\s+ask\b`)

// lastFooter is the latest start at which re matches through the end of
// body (re is anchored at $ and swallows the lines after its start).
func lastFooter(re *regexp.Regexp, body string) []int {
	var last []int
	for from := 0; from < len(body); {
		loc := re.FindStringIndex(body[from:])
		if loc == nil {
			break
		}
		last = []int{from + loc[0], len(body)}
		from += loc[0] + 1
	}
	return last
}

func isWebNote(p string) bool {
	for _, re := range webNotes {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// modelPlaceholder replaces a self-identifying name or phrase.
const modelPlaceholder = "[model]"

// selfNames matches vendor and model names and "as an AI model" phrases,
// with the version and tier words that follow a model name.
var selfNames = regexp.MustCompile(`(?i)\bas an? (?:AI|artificial intelligence)(?: language)? model\b` +
	`|\bas a large language model\b` +
	`|\b(?:Claude|Gemini|Grok|Llama|Copilot|Perplexity|DeepSeek|Qwen|Mistral|Kimi|Codex)(?:[ -](?:\d+(?:\.\d+)*|Opus|Sonnet|Haiku|Fable|Pro|Flash|Ultra|Nano|Mini|Code|Heavy))*\b` +
	`|\b(?:ChatGPT|GPT-?\d[\w.]*|o\d-(?:mini|pro))(?:[ -](?:mini|turbo|pro|Thinking))*\b` +
	`|\b(?:Anthropic|OpenAI|Google|DeepMind|xAI|Meta AI|Microsoft|Moonshot)\b`)

// redact replaces self-identifying names and phrases in an answer with
// modelPlaceholder. Writing style can still give a member away.
func redact(s string) string {
	return selfNames.ReplaceAllString(s, modelPlaceholder)
}

// reviewerCopy is an answer as reviewers and the chairman see it: without
// a web agent's tail (web is whether the member is a web agent) and with
// self-identification redacted. The report keeps the full reply.
func reviewerCopy(body string, web bool) string {
	if web {
		body = stripWebTail(body)
	}
	return redact(strings.TrimSpace(body))
}
