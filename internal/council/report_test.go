package council

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	xssScript  = `<script>alert(1)</script>`
	xssOnerror = `<img src=x onerror="alert(2)">`
	xssLink    = `<a href="javascript:alert(3)">click</a>`
)

func TestReportEscapesModelText(t *testing.T) {
	f := completedCouncil()
	f.Question = "Is this safe? " + xssScript
	f.Outcome.Answers[0].Body = "Answer with " + xssScript
	f.Outcome.Answers[1].Body = "Answer with " + xssOnerror
	f.Outcome.Answers[2].Body = "Answer with " + xssLink + " and [md](javascript:alert(4))"
	f.Outcome.Verdict.Recommendation = "Pick Answer B " + xssOnerror
	f.Outcome.Absent[0].Detail = xssLink

	var buf bytes.Buffer
	if err := RenderReport(&buf, f); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	for _, bad := range []string{xssScript, xssOnerror, xssLink, "<script", "<img", `href="javascript:`} {
		if strings.Contains(html, bad) {
			t.Errorf("report contains unescaped %q", bad)
		}
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;img src=x onerror=", "&lt;a href="} {
		if !strings.Contains(html, want) {
			t.Errorf("report lacks escaped text %q", want)
		}
	}

	csp := regexp.MustCompile(`<meta http-equiv="Content-Security-Policy" content="([^"]*)">`).FindStringSubmatch(html)
	if csp == nil {
		t.Fatal("report has no Content-Security-Policy meta tag")
	}
	for _, directive := range []string{"default-src 'none'", "script-src 'none'"} {
		if !strings.Contains(csp[1], directive) {
			t.Errorf("CSP %q lacks %q", csp[1], directive)
		}
	}
	// No tag loads or links anything, and the CSS imports nothing.
	if m := regexp.MustCompile(`(?i)<[^>]*\s(src|srcset|href|action|data)\s*=`).FindString(html); m != "" {
		t.Errorf("report has a tag referencing a resource: %s", m)
	}
	for _, ref := range []string{"<link", "@import", "url(", "<iframe", "<object", "<embed"} {
		if strings.Contains(strings.ToLower(html), ref) {
			t.Errorf("report references an external resource via %q", ref)
		}
	}
}

func TestReportRevealsAuthorshipAndNotes(t *testing.T) {
	f := completedCouncil()
	f.Outcome.Truncated = []string{"grok-web"}
	f.Outcome.Context = []ContextNote{{File: "plan.md", Member: "claude-web", Delivery: DeliveryCut}}
	f.Outcome.Verdict.Failed = []Absence{{Member: "chatgpt-web", Stage: StageChairman, Reason: AbsentNoVerdict}}

	var buf bytes.Buffer
	if err := RenderReport(&buf, f); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{
		"Answer B (gemini-web)", // chairman labels revealed
		"Keep one node and add a read replica.",
		"copilot-web", AbsentTimedOut, // absent member with reason
		"codex", "excluded (in chain)", // excluded member with reason
		"plan.md", DeliveryCut, // attachment note
		"cut to fit", // truncation note
		"Writing style can still give a model away",
		"3m12s", // stage timing
		"github.com/mvanhorn/agent-tincan",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report lacks %q", want)
		}
	}
}

func TestReportVerdictUnavailable(t *testing.T) {
	f := completedCouncil()
	f.Outcome.Verdict = Verdict{Category: Uncategorized, Unavailable: true, Reason: "no chairman candidate delivered a verdict within 4m0s"}
	var buf bytes.Buffer
	if err := RenderReport(&buf, f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Verdict unavailable") {
		t.Error("report does not say the verdict is unavailable")
	}
}

func TestSaveArtifactsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	report, card, err := SaveArtifacts(dir, completedCouncil())
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("report dir mode = %o, want 700", st.Mode().Perm())
	}
	for _, p := range []string{report, card} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", p, st.Mode().Perm())
		}
		if filepath.Dir(p) != dir {
			t.Errorf("%s is not in %s", p, dir)
		}
	}
}

func TestSaveArtifactsKeepsReportWhenCardFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "reports")
	f := completedCouncil()
	blocked := filepath.Join(dir, artifactBase(f.At, f.RequestID)+".png")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	report, card, err := SaveArtifacts(dir, f)
	if err == nil {
		t.Fatal("SaveArtifacts succeeded with a directory at the card path")
	}
	if card != "" {
		t.Errorf("card path = %q, want empty", card)
	}
	if report != filepath.Join(dir, artifactBase(f.At, f.RequestID)+".html") {
		t.Fatalf("report path = %q, want the saved report", report)
	}
	if _, err := os.Stat(report); err != nil {
		t.Errorf("report not on disk: %v", err)
	}
}
