package history

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	grokCLI1 = "01a0f000-0000-7000-8000-000000000001" // owner, ~/code/x, two prompts, image
	grokCLI2 = "01a0f000-0000-7000-8000-000000000002" // owner, ~/code/x, string prompt, no title
	grokCLI3 = "01a0f000-0000-7000-8000-000000000003" // wake run, recorded id
	grokCLI4 = "01a0f000-0000-7000-8000-000000000004" // timed-out wake run in the workdir, not recorded
	grokCLI5 = "01a0f000-0000-7000-8000-000000000005" // recorded wake id in an ordinary directory
	grokCLI6 = "01a0f000-0000-7000-8000-000000000006" // forty days old
)

// grokCLIFixture copies testdata/grok-cli: sessions/ is the owner's
// $GROK_HOME/sessions, tincan/ the directory holding the wake's recorded
// session list.
func grokCLIFixture(t *testing.T) *GrokCLI {
	t.Helper()
	root := copyTree(t, "grok-cli")
	return &GrokCLI{
		Home:       root,
		WakeDir:    filepath.Join(root, "tincan"),
		ScratchDir: fixtureScratch,
		Now:        func() time.Time { return fixtureNow },
	}
}

func TestGrokCLIListNewestFirstWithoutWakeRuns(t *testing.T) {
	r := grokCLIFixture(t)
	page, err := r.List(context.Background(), 20, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// 3, 4 and 5 are wake runs; 6 is past the 30-day window.
	if got := ids(page.Conversations); got != grokCLI1+","+grokCLI2 {
		t.Fatalf("list = %s, want 1,2", got)
	}
	c := page.Conversations[0]
	if c.Source != SourceGrokCLI || c.Title != "Parser refactor plan" || c.Cwd != "/Users/matt/code/x" || c.Automated {
		t.Fatalf("first = %+v", c)
	}
	if page.Conversations[1].Title != "rename the config loader" {
		t.Fatalf("untitled session title = %q, want its first prompt", page.Conversations[1].Title)
	}
	if page.Limited != LimitAge {
		t.Fatalf("limited = %q, want age (session 6 is past the window)", page.Limited)
	}
}

func TestGrokCLIListAllMarksEveryWakeRunAutomated(t *testing.T) {
	r := grokCLIFixture(t)
	page, err := r.List(context.Background(), 20, Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page.Conversations); got != grokCLI4+","+grokCLI3+","+grokCLI5+","+grokCLI1+","+grokCLI2 {
		t.Fatalf("list --all = %s", got)
	}
	auto := map[string]bool{}
	for _, c := range page.Conversations {
		auto[c.ID] = c.Automated
	}
	// 3 and 5 by the recorded list; 4, which the timeout killed before it
	// could be recorded, by the wake's workdir.
	for id, want := range map[string]bool{grokCLI1: false, grokCLI2: false, grokCLI3: true, grokCLI4: true, grokCLI5: true} {
		if auto[id] != want {
			t.Errorf("%s automated = %v, want %v", id, auto[id], want)
		}
	}
}

func TestGrokCLILatestReadsPromptsAndRepliesSkippingInjectedEntries(t *testing.T) {
	r := grokCLIFixture(t)
	got, err := convsOf(r.Read(context.Background(), Query{Source: SourceGrokCLI, Mode: ModeLatest, Count: 2, WantImages: true}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != grokCLI1 || got[1].ID != grokCLI1 {
		t.Fatalf("latest 2 = %s, want both turns of session 1", ids(got))
	}
	first, second := got[0].Messages, got[1].Messages
	if len(first) != 2 || first[0].Text != "does this diagram match" || first[1].Text != "Yes, the diagram shows the two passes." {
		t.Fatalf("newest turn = %+v", first)
	}
	if len(first[0].Images) != 1 || first[0].Images[0].MIME != "image/png" {
		t.Fatalf("newest prompt images = %+v", first[0].Images)
	}
	want := time.Date(2026, 9, 21, 9, 59, 0, 0, time.UTC)
	if !first[0].Time.Equal(want) {
		t.Fatalf("prompt time = %v, want %v from prompt_history.jsonl", first[0].Time, want)
	}
	// The reply is the last thing the model said in the turn, not its
	// narration before a tool call.
	if len(second) != 2 || second[0].Text != "plan the parser refactor" || second[1].Text != "Split the lexer out first, then the AST." {
		t.Fatalf("older turn = %+v", second)
	}
	for _, c := range got {
		for _, m := range c.Messages {
			for _, bad := range []string{"<user_info>", "<system-reminder>", "<project_instructions>", "coding agent", "package parser"} {
				if strings.Contains(m.Text, bad) {
					t.Fatalf("injected or tool text %q leaked into %q", bad, m.Text)
				}
			}
		}
	}
}

func TestGrokCLISearchAndConversation(t *testing.T) {
	r := grokCLIFixture(t)
	got, err := convsOf(r.Read(context.Background(), Query{Source: SourceGrokCLI, Mode: ModeSearch, Terms: []string{"config", "loader"}}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if ids(got) != grokCLI2 || got[0].Messages[1].Text != "Renamed it to loadConfig." {
		t.Fatalf("search = %+v", got)
	}
	got, err = convsOf(r.Read(context.Background(), Query{Source: SourceGrokCLI, Mode: ModeConversation, ConversationID: grokCLI1}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Messages) != 4 {
		t.Fatalf("conversation = %+v", got)
	}
	// A wake run is not found without all, and is with it.
	if _, err := r.Read(context.Background(), Query{Source: SourceGrokCLI, Mode: ModeConversation, ConversationID: grokCLI3}, Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wake run by id without all: err = %v, want not found", err)
	}
	got, err = convsOf(r.Read(context.Background(), Query{Source: SourceGrokCLI, Mode: ModeConversation, ConversationID: grokCLI3}, Options{All: true}))
	if err != nil || len(got) != 1 || !got[0].Automated {
		t.Fatalf("wake run by id with all: %+v, %v", got, err)
	}
}

// writeGrokSession adds one session under home/sessions for the marker
// tests.
func writeGrokSession(t *testing.T, home, cwd, id string, summary map[string]any) {
	t.Helper()
	dir := filepath.Join(home, "sessions", strings.ReplaceAll(url.QueryEscape(cwd), "+", "%20"), id)
	mkdirs(t, dir)
	s := map[string]any{"info": map[string]any{"id": id, "cwd": cwd}, "created_at": "2026-09-21T08:00:00Z", "updated_at": "2026-09-21T08:00:10Z"}
	maps.Copy(s, summary)
	raw, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	chat := `{"type":"user","content":"hello from ` + id + `","prompt_index":0}` + "\n" + `{"type":"assistant","content":"hi"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "chat_history.jsonl"), []byte(chat), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// Sessions under a wake home (a <name>.wake directory beside the tincan
// configs), or started with the wake's sandbox profile or its GROK_HOME,
// are wake runs even with nothing recorded.
func TestGrokCLIWakeHomeProfileAndGrokHomeMarkRunsAutomated(t *testing.T) {
	home, wakeDir := t.TempDir(), t.TempDir()
	wakeHome := filepath.Join(wakeDir, "grok-helper.wake")
	mkdirs(t, filepath.Join(wakeHome, ".grok"))
	const (
		own     = "01a0f000-0000-7000-8000-000000000101"
		inHome  = "01a0f000-0000-7000-8000-000000000102"
		profile = "01a0f000-0000-7000-8000-000000000103"
		grokDir = "01a0f000-0000-7000-8000-000000000104"
	)
	writeGrokSession(t, home, "/Users/matt/code/z", own, map[string]any{"sandbox_profile": "off"})
	writeGrokSession(t, home, filepath.Join(wakeHome, "elsewhere"), inHome, nil)
	writeGrokSession(t, home, "/Users/matt/code/z", profile, map[string]any{"sandbox_profile": GrokWakeSandboxProfile})
	writeGrokSession(t, home, "/Users/matt/code/z", grokDir, map[string]any{"grok_home": filepath.Join(wakeHome, ".grok")})
	r := &GrokCLI{Home: home, WakeDir: wakeDir, Now: func() time.Time { return fixtureNow }}
	page, err := r.List(context.Background(), 20, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ids(page.Conversations) != own {
		t.Fatalf("list = %s, want only the owner's session", ids(page.Conversations))
	}
	page, err = r.List(context.Background(), 20, Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range page.Conversations {
		if c.Automated != (c.ID != own) {
			t.Errorf("%s automated = %v", c.ID, c.Automated)
		}
	}
	if len(page.Conversations) != 4 {
		t.Fatalf("list --all = %s", ids(page.Conversations))
	}
}

// A missing or empty sessions directory is "no history", naming the
// directory, never a raw file-not-found error.
func TestGrokCLINoHistory(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".grok")
	r := &GrokCLI{Home: home}
	for _, setup := range []func(){func() {}, func() { mkdirs(t, filepath.Join(home, "sessions")) }} {
		setup()
		_, err := r.Read(context.Background(), Query{Source: SourceGrokCLI, Mode: ModeLatest, Count: 1}, Options{})
		want := "no Grok CLI history found in " + filepath.Join(home, "sessions")
		if !errors.Is(err, ErrNoHistory) || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if got := readFailure(Query{Source: SourceGrokCLI}, err); got != "No Grok CLI history was found on this machine." {
			t.Fatalf("reply = %q", got)
		}
	}
}

// NewGrokCLI reads the owner's $GROK_HOME, ~/.grok by default.
func TestNewGrokCLIHonorsGrokHome(t *testing.T) {
	t.Setenv("GROK_HOME", "/tmp/somewhere/.grok")
	if r := NewGrokCLI(); r.Home != "/tmp/somewhere/.grok" || r.Source() != SourceGrokCLI {
		t.Fatalf("reader = %+v", r)
	}
	t.Setenv("GROK_HOME", "")
	h, _ := os.UserHomeDir()
	if r := NewGrokCLI(); r.Home != filepath.Join(h, ".grok") {
		t.Fatalf("default home = %q", r.Home)
	}
}

// The query step tells Grok CLI apart from Grok on the web, and both the
// schema and the structured-query help name every source.
func TestExtractorRoutesGrokCLIApartFromGrok(t *testing.T) {
	for _, want := range []string{
		`"grok-cli" for the Grok CLI`,
		`"grok" for Grok or grok.com`,
	} {
		if !strings.Contains(extractInstructions, want) {
			t.Errorf("extractor instructions lack %q", want)
		}
	}
	// "what did I ask Grok CLI" must not read as the web source: the grok
	// rule says the CLI is grok-cli.
	if i, j := strings.Index(extractInstructions, `"grok" for`), strings.Index(extractInstructions, "not the Grok CLI"); i < 0 || j < i {
		t.Errorf("the grok rule does not exclude the CLI:\n%s", extractInstructions)
	}
	var schema struct {
		Properties struct {
			Source struct {
				Enum []string `json:"enum"`
			} `json:"source"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(extractSchema), &schema); err != nil {
		t.Fatal(err)
	}
	for _, s := range Sources {
		found := false
		for _, e := range schema.Properties.Source.Enum {
			found = found || e == string(s)
		}
		if !found {
			t.Errorf("schema enum lacks %q", s)
		}
		if !strings.Contains(structuredHelp, string(s)) {
			t.Errorf("structured help lacks %q", s)
		}
	}
	q, err := parseExtraction([]byte(`{"source":"grok-cli","mode":"latest","terms":[],"conversation_id":"","count":1,"want_images":false,"with_images":false}`))
	if err != nil || q.Source != SourceGrokCLI {
		t.Fatalf("grok-cli extraction = %+v, %v", q, err)
	}
	q, err = parseExtraction([]byte(`{"source":"grok","mode":"latest","terms":[],"conversation_id":"","count":1,"want_images":false,"with_images":false}`))
	if err != nil || q.Source != SourceGrok {
		t.Fatalf("grok extraction = %+v, %v", q, err)
	}
}
