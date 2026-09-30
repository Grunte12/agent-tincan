package council

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
)

//go:embed templates/report.html.tmpl
var reportFS embed.FS

// reportTmpl is the report page. Every piece of model or convener text is
// inserted as escaped text: there is no template.HTML and no markdown
// rendering, and the page's Content-Security-Policy blocks scripts and
// every remote load.
var reportTmpl = template.Must(template.New("report.html.tmpl").ParseFS(reportFS, "templates/report.html.tmpl"))

// RepoURL is the project link on the report and the cards.
const RepoURL = "github.com/mvanhorn/agent-tincan"

// reportView is the report's data, every string still plain text.
type reportView struct {
	Question, RequestID, Date, State, Reason, Status string
	Completed                                        bool
	Category, Chairman                               string
	VerdictShown, VerdictUnavailable                 bool
	VerdictReason                                    string
	Sections                                         []reportSection
	Labels                                           []labelView
	Ranking                                          []rankView
	Answers                                          []answerView
	Ballots                                          []ballotView
	Timings                                          []timingView
	Absent                                           []Absence
	Excluded                                         []Exclusion
	Context                                          []contextView
	Truncated                                        []string
	Repo                                             string
}

type reportSection struct{ Head, Text string }

type labelView struct{ Label, Member string }

type rankView struct {
	Placement int
	Member    string
	Score     string
	Ballots   int
	// Pct is the score bar's width, 0 to 100.
	Pct int
}

type answerView struct {
	Member, Placement, Elapsed, Body string
	Truncated                        bool
}

type ballotView struct {
	Reviewer string
	Valid    bool
	Ranked   []string
	Body     string
}

type timingView struct{ Stage, Took string }

type contextView struct {
	File    string
	Members []ContextNote
}

// RenderReport writes f's self-contained HTML report: the question, the
// verdict with authorship revealed, the peer ranking and every ballot,
// every answer, timings, absent and excluded members, and how context
// files reached each member.
func RenderReport(w io.Writer, f Finished) error {
	return reportTmpl.Execute(w, f.reportView())
}

func (f Finished) reportView() reportView {
	o := f.Outcome
	v := o.Verdict
	rv := reportView{
		Question:           f.Question,
		RequestID:          f.RequestID,
		Date:               f.At.Format("2006-01-02 15:04 MST"),
		State:              string(o.State),
		Reason:             o.Reason,
		Status:             f.statusLine(),
		Completed:          o.State == CouncilCompleted,
		VerdictShown:       f.verdictShown(),
		VerdictUnavailable: o.State == CouncilCompleted && v.Unavailable,
		VerdictReason:      v.Reason,
		Absent:             f.absences(),
		Excluded:           f.Excluded,
		Truncated:          o.Truncated,
		Repo:               RepoURL,
	}
	if f.At.IsZero() {
		rv.Date = ""
	}
	if rv.Completed {
		rv.Category = f.category()
	}
	if rv.VerdictShown {
		rv.Chairman = v.Chairman
		for _, s := range []reportSection{
			{"Recommendation", v.Recommendation}, {"Agreement", v.Agreement},
			{"Disagreement", v.Disagreement}, {"Minority", v.Minority},
		} {
			if s.Text != "" {
				rv.Sections = append(rv.Sections, reportSection{s.Head, revealLabels(s.Text, v.Labels)})
			}
		}
		for l, m := range v.Labels {
			rv.Labels = append(rv.Labels, labelView{l, m})
		}
		slices.SortFunc(rv.Labels, func(a, b labelView) int { return strings.Compare(a.Label, b.Label) })
	}
	placement := map[string]int{}
	for _, s := range o.Standings {
		placement[s.Member] = s.Placement
		rv.Ranking = append(rv.Ranking, rankView{
			Placement: s.Placement, Member: s.Member, Score: fmt.Sprintf("%.2f", s.Score),
			Ballots: s.Ballots, Pct: int(max(0, min(1, s.Score))*100 + 0.5),
		})
	}
	for _, a := range o.Answers {
		av := answerView{Member: a.Member, Body: a.Body, Truncated: slices.Contains(o.Truncated, a.Member), Placement: "not scored"}
		if p, ok := placement[a.Member]; ok {
			av.Placement = fmt.Sprintf("#%d", p)
		}
		if a.Elapsed > 0 {
			av.Elapsed = roundDuration(a.Elapsed)
		}
		rv.Answers = append(rv.Answers, av)
	}
	for _, r := range o.Reviews {
		bv := ballotView{Reviewer: r.Reviewer, Valid: r.Valid, Body: r.Body}
		for _, m := range r.Ranked {
			if m == r.Reviewer {
				m += " (own answer, not counted)"
			}
			bv.Ranked = append(bv.Ranked, m)
		}
		rv.Ballots = append(rv.Ballots, bv)
	}
	for _, t := range []struct {
		stage string
		took  time.Duration
	}{{"Answers", o.AnswerTime}, {"Peer review", o.ReviewTime}, {"Chairman", o.ChairTime}} {
		if t.took > 0 {
			rv.Timings = append(rv.Timings, timingView{t.stage, roundDuration(t.took)})
		}
	}
	for _, n := range o.Context {
		i := slices.IndexFunc(rv.Context, func(c contextView) bool { return c.File == n.File })
		if i < 0 {
			rv.Context = append(rv.Context, contextView{File: n.File})
			i = len(rv.Context) - 1
		}
		rv.Context[i].Members = append(rv.Context[i].Members, n)
	}
	return rv
}

func roundDuration(d time.Duration) string { return d.Round(time.Second).String() }

// SaveArtifacts writes f's HTML report and PNG scorecard into dir and
// returns their paths. dir is created 0700 and the files 0600, since they
// carry model answers and the convener's question. If the report is saved
// but the scorecard is not, the report path is still returned with the error.
func SaveArtifacts(dir string, f Finished) (reportPath, cardPath string, err error) {
	base := artifactBase(f.At, f.RequestID)
	var report, card bytes.Buffer
	if err := RenderReport(&report, f); err != nil {
		return "", "", fmt.Errorf("council report: %w", err)
	}
	if err := DrawScorecard(&card, f); err != nil {
		return "", "", fmt.Errorf("council scorecard: %w", err)
	}
	reportPath = filepath.Join(dir, base+".html")
	cardPath = filepath.Join(dir, base+".png")
	if err := writePrivate(reportPath, report.Bytes()); err != nil {
		return "", "", err
	}
	if err := writePrivate(cardPath, card.Bytes()); err != nil {
		return reportPath, "", err
	}
	return reportPath, cardPath, nil
}

// SaveLeaderboardCard draws the leaderboard card for category ("" is
// overall) into dir and returns its path, owner-only like the reports.
// Each category has one file, replaced on every request, so the folder
// does not grow with each leaderboard asked for.
func SaveLeaderboardCard(dir string, rows []LeaderboardRow, category string, at time.Time) (string, error) {
	var card bytes.Buffer
	if err := DrawLeaderboard(&card, rows, category, at); err != nil {
		return "", fmt.Errorf("council leaderboard card: %w", err)
	}
	name := "leaderboard"
	if category != "" {
		name += "-" + safeName(category)
	}
	path := filepath.Join(dir, name+".png")
	return path, writePrivate(path, card.Bytes())
}

// artifactBase is a report's file name without extension: the time
// convened, then the request id made safe for a file name.
func artifactBase(at time.Time, requestID string) string {
	return at.Format("20060102-150405") + "-" + safeName(requestID)
}

func safeName(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, s)
	if out == "" {
		out = "council"
	}
	return out
}

// writePrivate writes data to path with owner-only permissions: a folder
// it creates is 0700, and the file is 0600 even when it already existed.
// An existing folder keeps its mode, since the owner may point report_dir
// at a folder of their own.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("council artifacts: %w", err)
	}
	if err := client.WritePrivateFile(path, data); err != nil {
		return fmt.Errorf("council artifacts: %w", err)
	}
	return nil
}
