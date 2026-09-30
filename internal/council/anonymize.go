package council

import (
	"regexp"
	"strings"
)

// Web agents end each reply with a tail after the answer text: an
// optional truncation notice, a "Sources:" list, notes, the
// "<Site> conversation: <id>" footer, and image lines. The footer and
// notes name the site, so reviewers get the answer text only.
var (
	webFooter = regexp.MustCompile(`\n\n[^\n]{1,40} conversation: \S+(?:\n[^\n]*)*$`)
	webNotes  = []*regexp.Regexp{
		regexp.MustCompile(`^Sources:\n`),
		regexp.MustCompile(`^\(reply truncated: showing \d+ of \d+ bytes\)$`),
		regexp.MustCompile(`^Your previous .{1,40} conversation \(id [^)]*\) .*new chat\.$`),
		regexp.MustCompile(`^.{1,40}'s reply had \d+ images? that could not be attached; open the conversation to see (?:it|them)\.$`),
	}
)

// stripWebTail returns a web agent's reply without its tail. A reply
// without the footer is returned as is.
func stripWebTail(body string) string {
	loc := webFooter.FindStringIndex(body)
	if loc == nil {
		return body
	}
	paras := strings.Split(body[:loc[0]], "\n\n")
	for len(paras) > 1 && isWebNote(paras[len(paras)-1]) {
		paras = paras[:len(paras)-1]
	}
	return strings.TrimSpace(strings.Join(paras, "\n\n"))
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
