package council

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "council.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMissingConfigIsTheDefault(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "council.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Chairmen, []string{"claude-web", "chatgpt-web", "gemini-web"}) {
		t.Fatalf("chairmen %v", cfg.Chairmen)
	}
	if cfg.AnswerLimit != 6*time.Minute || cfg.ReviewLimit != 5*time.Minute || cfg.ChairmanLimit != 4*time.Minute {
		t.Fatalf("limits %v %v %v", cfg.AnswerLimit, cfg.ReviewLimit, cfg.ChairmanLimit)
	}
	if !slices.Contains(cfg.Categories, "architecture") || !slices.Contains(cfg.Categories, "debugging") {
		t.Fatalf("categories %v", cfg.Categories)
	}
	if cfg.Members != nil || cfg.Exclude != nil || cfg.Dir != "" || cfg.ReportDir != "" {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
}

func TestLoadConfig(t *testing.T) {
	p := writeConfig(t, `{
		"members": ["claude-code"],
		"exclude": ["perplexity-web"],
		"chairmen": ["gemini-web", "claude-web"],
		"categories": ["architecture", "writing"],
		"answer_limit": "3m",
		"review_limit": "90s",
		"chairman_limit": "7m",
		"dir": "/srv/council",
		"report_dir": "/srv/council/out"
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Members, []string{"claude-code"}) || !slices.Equal(cfg.Exclude, []string{"perplexity-web"}) ||
		!slices.Equal(cfg.Chairmen, []string{"gemini-web", "claude-web"}) || !slices.Equal(cfg.Categories, []string{"architecture", "writing"}) ||
		cfg.AnswerLimit != 3*time.Minute || cfg.ReviewLimit != 90*time.Second || cfg.ChairmanLimit != 7*time.Minute ||
		cfg.Dir != "/srv/council" || cfg.ReportDir != "/srv/council/out" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	for _, body := range []string{
		`{"model":"x"}`,
		`{"chairmen":["a"],"chairmen":["b"]}`,
		`{"members":null}`,
		`null`,
		`{} {}`,
		`{"chairmen":[]}`,
		`{"categories":[]}`,
		`{"categories":["Debugging"]}`,
		`{"categories":["uncategorized"]}`,
		`{"categories":["a","a"]}`,
		`{"members":[""]}`,
		`{"members":["codex"],"exclude":["codex"]}`,
		`{"answer_limit":"0s"}`,
		`{"review_limit":"8m"}`,
		`{"chairman_limit":"soon"}`,
		`{"dir":"relative/path"}`,
	} {
		if cfg, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("%s: loaded as %+v", body, cfg)
		}
	}
}

func TestDefaultPaths(t *testing.T) {
	if got := DefaultDir("darwin", "/Users/m"); got != "/Users/m/Library/Application Support/tincan-council" {
		t.Fatalf("darwin dir %q", got)
	}
	if got := DefaultDir("linux", "/home/m"); got != "/home/m/.local/share/tincan-council" {
		t.Fatalf("linux dir %q", got)
	}
	if got := ConfigPathIn("/d"); got != "/d/council.json" {
		t.Fatalf("config path %q", got)
	}
}
