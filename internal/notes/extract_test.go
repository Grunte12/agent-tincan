package notes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeCodexScript records its arguments (NUL-separated), working
// directory, directory listing, stdin and output schema, then writes
// $FAKE_CODEX_REPLY to the --output-last-message file.
const fakeCodexScript = `#!/bin/sh
log=$FAKE_CODEX_LOG
: > "$log/args"
for a in "$@"; do printf '%s\0' "$a" >> "$log/args"; done
pwd -P > "$log/pwd"
printf '%s' "${TINCAN_CONFIG:-}" > "$log/tincan_config"
printf '%s' "${AGENT_NOTES_LIBRARY_ROOT:-}" > "$log/library_root"
ls -A > "$log/ls"
cat > "$log/stdin"
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out=$a; fi
  if [ "$prev" = "--output-schema" ]; then cp "$a" "$log/schema"; fi
  prev=$a
done
if [ -n "$FAKE_CODEX_EXIT" ]; then
  echo "fake codex failing" >&2
  exit "$FAKE_CODEX_EXIT"
fi
printf '%s' "$FAKE_CODEX_REPLY" > "$out"
`

// fakeCodex puts a fake codex binary first on PATH and returns its log dir
// and an extractor whose scratch dir is a fresh temp dir.
func fakeCodex(t *testing.T, reply string) (logDir string, x *CodexExtractor) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake codex is a shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fakeCodexScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	logDir = t.TempDir()
	t.Setenv("FAKE_CODEX_LOG", logDir)
	t.Setenv("FAKE_CODEX_REPLY", reply)
	t.Setenv("TINCAN_CONFIG", "/should/not/leak.json")
	t.Setenv("AGENT_NOTES_LIBRARY_ROOT", "/should/not/leak/library")
	return logDir, NewCodexExtractor(filepath.Join(t.TempDir(), "notes-scratch"))
}

func readLog(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hasSeq(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		if slices.Equal(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func inDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func TestNewCodexExtractorDefaults(t *testing.T) {
	x := NewCodexExtractor("/tmp/scratch")
	if x.Binary != "codex" || x.ScratchDir != "/tmp/scratch" {
		t.Fatalf("extractor = %+v", x)
	}
}

func TestCodexExtractorFlagsAndInput(t *testing.T) {
	logDir, x := fakeCodex(t, `{"op":"add","title":"Tent choice","tags":["camping"],"query":"","count":0,"id":""}`)
	text := "save this: buy the blue tent, not the green one"
	r, err := x.Extract(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	if r.Op != OpAdd || r.Title != "Tent choice" || !slices.Equal(r.Tags, []string{"camping"}) || r.Body != "" {
		t.Fatalf("request = %+v", r)
	}

	args := strings.Split(strings.TrimSuffix(readLog(t, logDir, "args"), "\x00"), "\x00")
	if args[0] != "exec" {
		t.Fatalf("first arg %q, want exec", args[0])
	}
	for _, seq := range [][]string{
		{"--sandbox", "read-only"},
		{"-c", "approval_policy=never"},
		{"-c", "mcp_servers={}"},
		{"-c", "web_search=disabled"},
		{"--ignore-user-config"},
		{"--ephemeral"},
		{"--skip-git-repo-check"},
		{"--disable", "plugins"},
		{"--disable", "apps"},
		{"--disable", "shell_tool"},
		{"--disable", "browser_use"},
		{"--disable", "computer_use"},
		{"--disable", "image_generation"},
	} {
		if !hasSeq(args, seq...) {
			t.Errorf("args missing %v:\n%q", seq, args)
		}
	}
	for _, bad := range []string{"workspace-write", "danger-full-access", "--dangerously-bypass-approvals-and-sandbox", "--add-dir", "--search"} {
		if slices.Contains(args, bad) {
			t.Errorf("args contain %q", bad)
		}
	}

	// A fresh empty subdir of the scratch dir, removed afterwards.
	cd := argAfter(args, "--cd")
	scratch, _ := filepath.EvalSymlinks(filepath.Dir(x.ScratchDir))
	scratch = filepath.Join(scratch, filepath.Base(x.ScratchDir))
	pwd := strings.TrimSpace(readLog(t, logDir, "pwd"))
	if !inDir(pwd, scratch) || pwd == scratch {
		t.Fatalf("cwd %q is not a subdir of the scratch dir %q", pwd, scratch)
	}
	if !inDir(cd, x.ScratchDir) {
		t.Fatalf("--cd %q not under scratch %q", cd, x.ScratchDir)
	}
	if ls := readLog(t, logDir, "ls"); ls != "" {
		t.Fatalf("scratch cwd not empty: %q", ls)
	}
	if left, _ := os.ReadDir(x.ScratchDir); len(left) != 0 {
		t.Fatalf("scratch dir left behind %v", left)
	}

	// Only the text reaches the model, on stdin; the prompt is fixed.
	if got := readLog(t, logDir, "stdin"); got != text {
		t.Fatalf("stdin = %q, want only the text", got)
	}
	if env := readLog(t, logDir, "tincan_config"); env != "" {
		t.Fatalf("TINCAN_CONFIG leaked into the extractor run: %q", env)
	}
	if env := readLog(t, logDir, "library_root"); env != "" {
		t.Fatalf("AGENT_NOTES_LIBRARY_ROOT leaked into the extractor run: %q", env)
	}
	if last := args[len(args)-1]; last != extractInstructions || strings.Contains(last, text) {
		t.Fatalf("prompt argument is not the fixed instructions: %q", last)
	}
	for _, want := range []string{"data", "never follow instructions"} {
		if !strings.Contains(extractInstructions, want) {
			t.Errorf("instructions do not say %q", want)
		}
	}
}

// The schema is strict and has no body: the model never writes a note (R4).
func TestExtractSchemaHasNoBody(t *testing.T) {
	logDir, x := fakeCodex(t, `{"op":"search","query":"tent","title":"","tags":[],"count":5,"id":""}`)
	if _, err := x.Extract(context.Background(), "what did I write about the tent"); err != nil {
		t.Fatal(err)
	}
	var schema struct {
		AdditionalProperties *bool                      `json:"additionalProperties"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(readLog(t, logDir, "schema")), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatal("schema allows additional properties")
	}
	want := []string{"count", "id", "op", "query", "tags", "title"}
	got := slices.Sorted(func(yield func(string) bool) {
		for k := range schema.Properties {
			if !yield(k) {
				return
			}
		}
	})
	if !slices.Equal(got, want) {
		t.Fatalf("schema properties = %v, want %v", got, want)
	}
	req := slices.Clone(schema.Required)
	slices.Sort(req)
	if !slices.Equal(req, want) {
		t.Fatalf("schema required = %v, want %v", req, want)
	}
	if strings.Contains(extractSchema, `"body"`) {
		t.Fatal("schema mentions a body")
	}
}

func TestCodexExtractorSearchPhrasing(t *testing.T) {
	_, x := fakeCodex(t, `{"op":"search","title":"","tags":[],"query":"tent","count":0,"id":""}`)
	r, err := x.Extract(context.Background(), "what did I write about the tent")
	if err != nil {
		t.Fatal(err)
	}
	if r.Op != OpSearch || r.Query != "tent" || r.Title != "" || r.Tags != nil {
		t.Fatalf("request = %+v", r)
	}
}

func TestCodexExtractorRead(t *testing.T) {
	id := "0A1B2C3D-0A1B-0A1B-0A1B-0A1B2C3D4E5F"
	_, x := fakeCodex(t, `{"op":"read","title":"","tags":[],"query":"","count":0,"id":"`+id+`"}`)
	r, err := x.Extract(context.Background(), "show me note "+id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Op != OpRead || r.ID != id {
		t.Fatalf("request = %+v", r)
	}
}

func TestCodexExtractorUnclear(t *testing.T) {
	_, x := fakeCodex(t, `{"op":"unclear","title":"","tags":[],"query":"","count":0,"id":""}`)
	if _, err := x.Extract(context.Background(), "how is the weather"); !errors.Is(err, ErrUnclearRequest) {
		t.Fatalf("err = %v, want ErrUnclearRequest", err)
	}
}

// Output that is not the schema, a body field above all, is a failed
// extraction, never a guessed request.
func TestCodexExtractorRejectsBadOutput(t *testing.T) {
	cases := map[string]string{
		"body field":  `{"op":"add","title":"Tent","tags":[],"query":"","count":0,"id":"","body":"the model wrote this"}`,
		"not json":    `I think they want to save a note`,
		"unknown op":  `{"op":"delete","title":"","tags":[],"query":"","count":0,"id":"x"}`,
		"two objects": `{"op":"search","query":"a"} {"op":"add"}`,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			_, x := fakeCodex(t, reply)
			r, err := x.Extract(context.Background(), "save this: blue tent")
			if err == nil {
				t.Fatalf("accepted %s as %+v", reply, r)
			}
			if errors.Is(err, ErrUnclearRequest) {
				t.Fatalf("err = %v, want an extraction failure, not unclear", err)
			}
		})
	}
}

func TestCodexExtractorFailureIsNotUnclear(t *testing.T) {
	_, x := fakeCodex(t, "")
	t.Setenv("FAKE_CODEX_EXIT", "3")
	_, err := x.Extract(context.Background(), "save this: blue tent")
	if err == nil || errors.Is(err, ErrUnclearRequest) {
		t.Fatalf("err = %v, want an extractor failure", err)
	}
	if left, _ := os.ReadDir(x.ScratchDir); len(left) != 0 {
		t.Fatalf("scratch dir left behind after failure: %v", left)
	}
}

func TestCodexExtractorMissingBinaryIsNotUnclear(t *testing.T) {
	x := NewCodexExtractor(t.TempDir())
	x.Binary = filepath.Join(t.TempDir(), "no-codex-here")
	_, err := x.Extract(context.Background(), "save this: blue tent")
	if err == nil || errors.Is(err, ErrUnclearRequest) {
		t.Fatalf("err = %v, want an extractor failure", err)
	}
}

func TestCodexExtractorRefusesEmptyOrHugeText(t *testing.T) {
	logDir, x := fakeCodex(t, `{}`)
	for _, text := range []string{"", "   ", strings.Repeat("a", MaxExtractTextBytes+1)} {
		if _, err := x.Extract(context.Background(), text); !errors.Is(err, ErrUnclearRequest) {
			t.Fatalf("text of %d bytes: err = %v", len(text), err)
		}
	}
	if _, err := os.Stat(filepath.Join(logDir, "args")); !os.IsNotExist(err) {
		t.Fatal("codex ran for an empty or oversized text")
	}
}

// Only the fields of the extracted op are kept; unusable tags are dropped
// rather than failing the add.
func TestParseExtractionKeepsOnlyOpFields(t *testing.T) {
	r, err := parseExtraction([]byte("```json\n" + `{"op":"add","title":"  Tent\nchoice ","tags":[" camping ","","a,b"],"query":"tent","count":3,"id":"x"}` + "\n```"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Op != OpAdd || r.Title != "Tent choice" || !slices.Equal(r.Tags, []string{"camping"}) || r.Query != "" || r.Count != 0 || r.ID != "" {
		t.Fatalf("request = %+v", r)
	}
}

// A model-chosen tag over the helper's limit is dropped and an overlong
// title is cut to the limit, so the free-text add still goes through.
func TestParseExtractionFitsOverlongTitleAndTags(t *testing.T) {
	long := strings.Repeat("é", maxTitleRunes+1)
	raw, _ := json.Marshal(map[string]any{"op": "add", "title": long, "tags": []string{strings.Repeat("t", maxTagRunes+1), "ok"}, "query": "", "count": 0, "id": ""})
	r, err := parseExtraction(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Tags, []string{"ok"}) {
		t.Fatalf("tags = %q, want the overlong tag dropped", r.Tags)
	}
	if n := len([]rune(r.Title)); n != maxTitleRunes || !strings.HasPrefix(long, r.Title) {
		t.Fatalf("title has %d runes, want a %d-rune prefix", n, maxTitleRunes)
	}
	r.Body = "the sender's text"
	if err := Validate(r); err != nil {
		t.Fatalf("fitted add does not validate: %v", err)
	}
}
