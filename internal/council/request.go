package council

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/history"
)

// Op is what a request asks Council to do.
type Op string

const (
	OpConvene     Op = "convene"
	OpLeaderboard Op = "leaderboard"
)

// Request is one parsed Council request. Only the fields of its Op are set.
type Request struct {
	Op       Op
	Question string
	// Members replaces the default roster; Exclude removes agents from it.
	// A form sets at most one of them.
	Members, Exclude []string
	// Chairman is tried before the council.json candidates.
	Chairman string
	// Category narrows a leaderboard; empty is overall.
	Category string
}

// RequestPrefix starts a structured request: "council:" then one JSON
// object.
const RequestPrefix = "council:"

// FormHelp is the fixed text that shows the structured form.
const FormHelp = `Anything without "council:" first is the question, asked of the default council. For more control put "council:" first and one JSON object after it. To convene: council: {"question":"...","members":["..."],"chairman":"..."} or council: {"question":"...","exclude":["..."]} (question required, the rest optional, members or exclude but not both). For the leaderboard: council: {"op":"leaderboard","category":"..."} (category optional).`

// wireRequest has a pointer per field, so a field that was sent can be
// told from one that was not.
type wireRequest struct {
	Op       *string   `json:"op"`
	Question *string   `json:"question"`
	Members  *[]string `json:"members"`
	Exclude  *[]string `json:"exclude"`
	Chairman *string   `json:"chairman"`
	Category *string   `json:"category"`
}

// ParseRequest reads a convening request body. Free text is the question
// with every default. A "council:" form is parsed strictly; a bad one is
// an error carrying the reason and FormHelp, for a declined reply.
func ParseRequest(body string, cfg Config) (Request, error) {
	raw, ok := history.StructuredBody(body, RequestPrefix)
	if !ok {
		q := strings.TrimSpace(body)
		if q == "" {
			return Request{}, fmt.Errorf("the question is empty. %s", FormHelp)
		}
		return Request{Op: OpConvene, Question: q}, nil
	}
	r, err := parseForm(raw, cfg)
	if err != nil {
		return Request{}, fmt.Errorf("bad council form: %v. %s", err, FormHelp)
	}
	return r, nil
}

func parseForm(raw string, cfg Config) (Request, error) {
	var w *wireRequest
	if err := decodeStrict([]byte(raw), &w); err != nil {
		return Request{}, err
	}
	if w == nil {
		return Request{}, errors.New("the form is null")
	}
	r := Request{Op: OpConvene}
	if w.Op != nil {
		r.Op = Op(*w.Op)
	}
	allowed := map[Op][]string{
		OpConvene:     {"question", "members", "exclude", "chairman"},
		OpLeaderboard: {"category"},
	}[r.Op]
	if allowed == nil {
		return Request{}, fmt.Errorf("op %q is not convene or leaderboard", r.Op)
	}
	sent := map[string]bool{"question": w.Question != nil, "members": w.Members != nil, "exclude": w.Exclude != nil, "chairman": w.Chairman != nil, "category": w.Category != nil}
	for _, f := range []string{"question", "members", "exclude", "chairman", "category"} {
		if sent[f] && !slices.Contains(allowed, f) {
			return Request{}, fmt.Errorf("%s is not a field of %s", f, r.Op)
		}
	}
	var err error
	switch r.Op {
	case OpConvene:
		if w.Question == nil {
			return Request{}, errors.New("question is required")
		}
		if r.Question = strings.TrimSpace(*w.Question); r.Question == "" {
			return Request{}, errors.New("question is empty")
		}
		if w.Members != nil && w.Exclude != nil {
			return Request{}, errors.New("use members or exclude, not both")
		}
		if w.Members != nil {
			if r.Members, err = names("members", *w.Members, false); err != nil {
				return Request{}, err
			}
		}
		if w.Exclude != nil {
			if r.Exclude, err = names("exclude", *w.Exclude, false); err != nil {
				return Request{}, err
			}
		}
		if w.Chairman != nil {
			if r.Chairman = *w.Chairman; r.Chairman == "" {
				return Request{}, errors.New("chairman is empty")
			}
		}
	case OpLeaderboard:
		if w.Category != nil {
			r.Category = *w.Category
			if r.Category != Uncategorized && !slices.Contains(cfg.Categories, r.Category) {
				return Request{}, fmt.Errorf("category %q is not one of %s, %s", r.Category, strings.Join(cfg.Categories, ", "), Uncategorized)
			}
		}
	}
	return r, nil
}
