// Package council is the Council service teammate: it puts one question to
// every model on the team, has them rank each other's answers blind, and
// has a chairman write the verdict.
package council

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

// Stage time limits. Each stays under the web agents' 8-minute send
// timeout, so a stage never outlasts the member it waits on.
const (
	DefaultAnswerLimit   = 6 * time.Minute
	DefaultReviewLimit   = 5 * time.Minute
	DefaultChairmanLimit = 4 * time.Minute
	maxStageLimit        = 8 * time.Minute
)

// Uncategorized is the leaderboard category of a council whose verdict
// never came back. It is reserved: council.json cannot list it.
const Uncategorized = "uncategorized"

// DefaultChairmen is the chairman candidate order when council.json names
// none: the most capable web models first.
var DefaultChairmen = []string{"claude-web", "chatgpt-web", "gemini-web"}

// DefaultCategories is the fixed set the chairman files each question
// under.
var DefaultCategories = []string{"architecture", "debugging", "writing", "research", "current-events", "other"}

// Config is council.json with defaults filled in.
type Config struct {
	// Members are agents the owner adds to the default roster. A listed
	// agent sits even when its kind is not eligible by default (a live
	// session, a service), and a form may name it.
	Members []string
	// Exclude are agents the owner leaves off the default roster. A form
	// may still name one.
	Exclude []string
	// Chairmen is the chairman candidate order. An entry names an agent,
	// or else every agent of that kind. Listing an agent here also lets a
	// form name it as chairman.
	Chairmen []string
	// Categories is the fixed set the chairman picks a category from.
	Categories []string
	// Stage time limits; the chairman limit covers failover too.
	AnswerLimit, ReviewLimit, ChairmanLimit time.Duration
	// Dir is the service's own folder (its database lives there) and
	// ReportDir where reports and cards are written. Empty means the
	// default: DefaultDir, and "reports" inside Dir.
	Dir, ReportDir string
}

// DefaultConfig is the configuration when there is no council.json.
func DefaultConfig() Config {
	return Config{
		Chairmen:      slices.Clone(DefaultChairmen),
		Categories:    slices.Clone(DefaultCategories),
		AnswerLimit:   DefaultAnswerLimit,
		ReviewLimit:   DefaultReviewLimit,
		ChairmanLimit: DefaultChairmanLimit,
	}
}

// DefaultDir is the service's own folder on goos: ~/Library/Application
// Support/tincan-council on macOS, else ~/.local/share/tincan-council.
func DefaultDir(goos, home string) string {
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "tincan-council")
	}
	return filepath.Join(home, ".local", "share", "tincan-council")
}

// ConfigPathIn is council.json inside dir. It lives in the service's
// folder, not beside the tincan client config, whose council.json is the
// service's relay identity.
func ConfigPathIn(dir string) string { return filepath.Join(dir, "council.json") }

// StorePathIn is the service's database inside its folder dir.
func StorePathIn(dir string) string { return filepath.Join(dir, "council.db") }

// Folders resolves the service's folders from its folder dir and cfg: the
// folder its database lives in, and where reports and cards are written.
func Folders(dir string, cfg Config) (data, reports string) {
	data = dir
	if cfg.Dir != "" {
		data = cfg.Dir
	}
	reports = filepath.Join(data, "reports")
	if cfg.ReportDir != "" {
		reports = cfg.ReportDir
	}
	return data, reports
}

// maxConfigBytes bounds how much of council.json is read.
const maxConfigBytes = 64 << 10

// wireConfig has a pointer per field, so an omitted field (default) can
// be told from one that was sent.
type wireConfig struct {
	Members       *[]string `json:"members"`
	Exclude       *[]string `json:"exclude"`
	Chairmen      *[]string `json:"chairmen"`
	Categories    *[]string `json:"categories"`
	AnswerLimit   *string   `json:"answer_limit"`
	ReviewLimit   *string   `json:"review_limit"`
	ChairmanLimit *string   `json:"chairman_limit"`
	Dir           *string   `json:"dir"`
	ReportDir     *string   `json:"report_dir"`
}

// LoadConfig reads council.json. A missing file is DefaultConfig. Only an
// omitted field takes its default: unknown, repeated, or null fields and
// out-of-range values are errors, so a typo never silently widens who
// sits on a council.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return Config{}, err
	}
	if len(b) > maxConfigBytes {
		return Config{}, errors.New("council.json: file too large")
	}
	cfg, err := parseConfig(b)
	if err != nil {
		return Config{}, fmt.Errorf("council.json: %w", err)
	}
	return cfg, nil
}

func parseConfig(b []byte) (Config, error) {
	var w *wireConfig
	if err := decodeStrict(b, &w); err != nil {
		return Config{}, err
	}
	if w == nil {
		return Config{}, errors.New("the file is null")
	}
	cfg := DefaultConfig()
	var err error
	if w.Members != nil {
		if cfg.Members, err = names("members", *w.Members, true); err != nil {
			return Config{}, err
		}
	}
	if w.Exclude != nil {
		if cfg.Exclude, err = names("exclude", *w.Exclude, true); err != nil {
			return Config{}, err
		}
	}
	for _, m := range cfg.Members {
		if slices.Contains(cfg.Exclude, m) {
			return Config{}, fmt.Errorf("%s is in both members and exclude", m)
		}
	}
	if w.Chairmen != nil {
		if cfg.Chairmen, err = names("chairmen", *w.Chairmen, false); err != nil {
			return Config{}, err
		}
	}
	if w.Categories != nil {
		if cfg.Categories, err = categories(*w.Categories); err != nil {
			return Config{}, err
		}
	}
	for _, l := range []struct {
		field string
		raw   *string
		dst   *time.Duration
	}{{"answer_limit", w.AnswerLimit, &cfg.AnswerLimit}, {"review_limit", w.ReviewLimit, &cfg.ReviewLimit}, {"chairman_limit", w.ChairmanLimit, &cfg.ChairmanLimit}} {
		if l.raw == nil {
			continue
		}
		d, err := time.ParseDuration(*l.raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: want a duration like \"6m\": %v", l.field, err)
		}
		if d <= 0 || d >= maxStageLimit {
			return Config{}, fmt.Errorf("%s must be above 0 and under %v", l.field, maxStageLimit)
		}
		*l.dst = d
	}
	for _, p := range []struct {
		field string
		raw   *string
		dst   *string
	}{{"dir", w.Dir, &cfg.Dir}, {"report_dir", w.ReportDir, &cfg.ReportDir}} {
		if p.raw == nil {
			continue
		}
		if !filepath.IsAbs(*p.raw) {
			return Config{}, fmt.Errorf("%s must be an absolute path", p.field)
		}
		*p.dst = *p.raw
	}
	return cfg, nil
}

// names checks a list of agent names: none blank, none twice, and not
// empty unless allowEmpty.
func names(field string, list []string, allowEmpty bool) ([]string, error) {
	if len(list) == 0 && !allowEmpty {
		return nil, fmt.Errorf("%s is empty", field)
	}
	for i, n := range list {
		if n == "" {
			return nil, fmt.Errorf("%s has an empty name", field)
		}
		if slices.Contains(list[:i], n) {
			return nil, fmt.Errorf("%s names %s twice", field, n)
		}
	}
	return list, nil
}

var categoryRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func categories(list []string) ([]string, error) {
	if len(list) == 0 {
		return nil, errors.New("categories is empty")
	}
	for i, c := range list {
		switch {
		case !categoryRE.MatchString(c):
			return nil, fmt.Errorf("category %q is not lowercase words joined by hyphens", c)
		case c == Uncategorized:
			return nil, fmt.Errorf("category %q is reserved", c)
		case slices.Contains(list[:i], c):
			return nil, fmt.Errorf("category %q is listed twice", c)
		}
	}
	return list, nil
}

// decodeStrict decodes exactly one JSON value with only whitespace after
// it into v, rejecting unknown fields and, when it is an object, repeated
// or null fields (the decoder would keep the last or the default).
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("not the council schema: %v", err)
	}
	if len(bytes.TrimSpace(b[dec.InputOffset():])) > 0 {
		return errors.New("text after the object")
	}
	dec = json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil // null; the caller reports it
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := t.(string)
		if seen[key] {
			return fmt.Errorf("%q is set more than once", key)
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%s is null", key)
		}
	}
	return nil
}
