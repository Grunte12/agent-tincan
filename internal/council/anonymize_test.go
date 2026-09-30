package council

import (
	"strings"
	"testing"
)

func TestStripWebTailRemovesFooterSourcesAndNotes(t *testing.T) {
	answer := "Use a queue.\n\nIt keeps order."
	for name, body := range map[string]string{
		"footer only":      answer + "\n\nChatGPT conversation: 68b1c2d3-aaaa",
		"sources":          answer + "\n\nSources:\n- [1] Queues https://example.com/q\n- [2] More https://example.com/m\n(and 3 more)\n\nPerplexity conversation: abc123",
		"notes and images": answer + "\n\nYour previous Gemini conversation (id c_1) was not found, so this went to a new chat.\n\nGemini's reply had 2 images that could not be attached; open the conversation to see them.\n\nGemini conversation: c_2\n1 image attached.",
		"truncated":        answer + "\n\n(reply truncated: showing 100 of 900 bytes)\n\nClaude conversation: abc",
	} {
		t.Run(name, func(t *testing.T) {
			if got := stripWebTail(body); got != answer {
				t.Fatalf("stripWebTail = %q, want %q", got, answer)
			}
		})
	}
	if got := stripWebTail(answer); got != answer {
		t.Fatalf("no tail: %q", got)
	}
}

func TestRedactSelfIdentification(t *testing.T) {
	for in, want := range map[string]string{
		"As Claude, I would pick B.":                  "As [model], I would pick B.",
		"As an AI language model, I cannot browse.":   "[model], I cannot browse.",
		"ChatGPT and GPT-4o from OpenAI agree.":       "[model] and [model] from [model] agree.",
		"Gemini 2.5 Pro by Google; Grok by xAI.":      "[model] by [model]; [model] by [model].",
		"Keep the claudette variable and the google_": "Keep the claudette variable and the google_",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReviewerCopyOnlyStripsWebReplies(t *testing.T) {
	body := "Answer.\n\nClaude conversation: abc"
	if got := reviewerCopy(body, true); got != "Answer." {
		t.Fatalf("web copy = %q", got)
	}
	if got := reviewerCopy(body, false); !strings.Contains(got, "conversation: abc") || strings.Contains(got, "Claude") {
		t.Fatalf("cli copy = %q", got)
	}
}
