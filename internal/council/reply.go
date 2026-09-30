package council

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Finished is a council that ran, or was declined before anyone was asked:
// what the reply, the report, and the scorecard are made from.
type Finished struct {
	RequestID string
	Question  string
	// Asked are the members the council asked, in seat order.
	Asked []string
	// Excluded are the roster agents that did not sit, with the reason.
	Excluded []Exclusion
	Outcome  Outcome
	// At is when the council was convened.
	At time.Time
	// ReportPath and CardPath are where the report and scorecard were
	// saved. The reply names them, so they are reachable on the owner's
	// machine even when attaching them fails.
	ReportPath, CardPath string
}

// Status is the reply status for the council's state: a completed council
// is answered, one without a quorum failed, and one that asked nobody
// (bad form, too-deep chain, too few eligible members) declined.
func (f Finished) Status() envelope.Status { return f.Outcome.State.replyStatus() }

// replyStatus is the reply status for a council that finished in state s.
func (s CouncilState) replyStatus() envelope.Status {
	switch s {
	case CouncilCompleted:
		return envelope.StatusAnswered
	case CouncilFailed:
		return envelope.StatusFailed
	}
	return envelope.StatusDeclined
}

// revealLabels names the author after every "Answer K" in the chairman's
// text, now that judging is over.
func revealLabels(s string, labels map[string]string) string {
	return replaceLabels(s, labels, func(ref, member string) string { return ref + " (" + member + ")" })
}

// firstLine is s's first non-empty line, trimmed.
func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// notScored counts the asked members without a peer score; for a council
// that never scored, the asked members that gave no answer.
func (f Finished) notScored() int {
	o := f.Outcome
	if o.State == CouncilCompleted {
		return len(f.Asked) - len(o.Standings)
	}
	return len(f.Asked) - len(o.Answers)
}

// statusLine is "7 asked, 5 scored, 2 absent, 1 excluded"; a council that
// never scored counts answers instead.
func (f Finished) statusLine() string {
	o := f.Outcome
	if o.State == CouncilCompleted {
		return fmt.Sprintf("%d asked, %d scored, %d absent, %d excluded", len(f.Asked), len(o.Standings), f.notScored(), len(f.Excluded))
	}
	return fmt.Sprintf("%d asked, %d answered, %d absent, %d excluded", len(f.Asked), len(o.Answers), f.notScored(), len(f.Excluded))
}

// absences is every member missing from a stage, the chairman candidates
// that gave no verdict last.
func (f Finished) absences() []Absence {
	return slices.Concat(f.Outcome.Absent, f.Outcome.Verdict.Failed)
}

// verdictShown is whether the council has a verdict to show.
func (f Finished) verdictShown() bool {
	return f.Outcome.State == CouncilCompleted && !f.Outcome.Verdict.Unavailable
}

// category is the leaderboard category: the chairman's, or uncategorized.
func (f Finished) category() string {
	if c := f.Outcome.Verdict.Category; c != "" && f.verdictShown() {
		return c
	}
	return Uncategorized
}

// Reply is the convener's reply to a finished council and its status. It
// is text first (the recommendation, the question's first line, a status
// line, the ranking, then the verdict's notes) and ends with a fenced
// council-result JSON block for scripts. A council that failed carries
// the answers it received.
func Reply(f Finished) (string, envelope.Status) {
	o := f.Outcome
	v := o.Verdict
	// A failed council carries the answers it received, cut to fit.
	var received []Answer
	if o.State == CouncilFailed {
		received = receivedAnswers(o.Answers)
	}
	// A completed council carries the chairman's verdict, cut to fit.
	var verdict []verdictSection
	if f.verdictShown() {
		verdict = f.verdictSections()
	}
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	switch {
	case o.State == CouncilDeclined:
		line("Council declined: %s", o.Reason)
	case o.State == CouncilFailed:
		line("Council failed: %s", o.Reason)
	case v.Unavailable:
		line("Verdict unavailable: %s. The ranking below is the blind peer review's.", v.Reason)
	default:
		line("Recommendation: %s", verdict[0].text)
	}
	if q := firstLine(f.Question); q != "" {
		line("Question: %s", q)
	}
	if o.State != CouncilDeclined {
		line("Status: %s", f.statusLine())
	}
	if f.verdictShown() {
		line("Chairman: %s. Category: %s.", v.Chairman, f.category())
	} else if o.State == CouncilCompleted {
		line("Category: %s.", Uncategorized)
	}

	if len(o.Standings) > 0 {
		line("")
		line("Ranking by blind peer review (mean Borda score, 0 to 1):")
		for _, s := range o.Standings {
			line("%d. %s %.2f (%d %s)", s.Placement, s.Member, s.Score, s.Ballots, plural(s.Ballots, "ballot", "ballots"))
		}
	}
	if f.verdictShown() {
		line("")
		for _, sec := range verdict[1:] {
			if sec.text != "" {
				line("%s: %s", sec.head, sec.text)
			}
		}
	}
	if abs := f.absences(); len(abs) > 0 {
		line("")
		line("Absent:")
		for _, a := range abs {
			line("- %s (%s stage): %s", a.Member, a.Stage, a.Reason)
		}
	}
	if len(f.Excluded) > 0 {
		line("")
		line("Excluded:")
		for _, x := range f.Excluded {
			line("- %s: %s", x.Name, x.Reason)
		}
	}
	if o.State == CouncilFailed && len(o.Answers) > 0 {
		line("")
		line("Answers received:")
		for _, a := range received {
			line("")
			line("== %s ==", a.Member)
			line("%s", strings.TrimSpace(a.Body))
		}
	}
	if f.ReportPath != "" || f.CardPath != "" {
		line("")
		if f.ReportPath != "" {
			line("Report: %s", f.ReportPath)
		}
		if f.CardPath != "" {
			line("Scorecard: %s", f.CardPath)
		}
	}

	block, _ := json.MarshalIndent(f.result(received, verdict), "", "  ")
	line("")
	line("%s", ResultFence)
	line("%s", block)
	line("```")
	return b.String(), f.Status()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ResultFence opens the reply's fenced council-result JSON block.
const ResultFence = "```council-result"

// councilResult is the council-result block.
type councilResult struct {
	RequestID string          `json:"request_id"`
	Status    envelope.Status `json:"status"`
	State     CouncilState    `json:"state"`
	Reason    string          `json:"reason,omitempty"`
	Question  string          `json:"question"`
	Asked     int             `json:"asked"`
	Category  string          `json:"category,omitempty"`
	Chairman  string          `json:"chairman,omitempty"`
	Verdict   *resultVerdict  `json:"verdict,omitempty"`
	// VerdictUnavailable is why a completed council has no verdict.
	VerdictUnavailable string         `json:"verdict_unavailable,omitempty"`
	Ranking            []resultRank   `json:"ranking,omitempty"`
	Answers            []resultAnswer `json:"answers,omitempty"`
	Absent             []resultAbsent `json:"absent,omitempty"`
	Excluded           []resultMember `json:"excluded,omitempty"`
	Report             string         `json:"report,omitempty"`
	Card               string         `json:"card,omitempty"`
}

type resultVerdict struct {
	Recommendation string `json:"recommendation"`
	Agreement      string `json:"agreement,omitempty"`
	Disagreement   string `json:"disagreement,omitempty"`
	Minority       string `json:"minority,omitempty"`
}

type resultRank struct {
	Member    string  `json:"member"`
	Placement int     `json:"placement"`
	Score     float64 `json:"score"`
	Ballots   int     `json:"ballots"`
}

type resultAnswer struct {
	Member string `json:"member"`
	Body   string `json:"body"`
}

type resultAbsent struct {
	Member string `json:"member"`
	Stage  string `json:"stage"`
	Reason string `json:"reason"`
}

type resultMember struct {
	Member string `json:"member"`
	Reason string `json:"reason"`
}

// result is the council-result block; received are a failed council's
// answers and verdict a completed council's verdict sections, as the reply
// carries them.
func (f Finished) result(received []Answer, verdict []verdictSection) councilResult {
	o := f.Outcome
	v := o.Verdict
	r := councilResult{
		RequestID: f.RequestID,
		Status:    f.Status(),
		State:     o.State,
		Question:  firstLine(f.Question),
		Asked:     len(f.Asked),
		Report:    f.ReportPath,
		Card:      f.CardPath,
	}
	if o.State != CouncilCompleted {
		r.Reason = o.Reason
	} else {
		r.Category = f.category()
	}
	switch {
	case f.verdictShown():
		r.Chairman = v.Chairman
		r.Verdict = &resultVerdict{
			Recommendation: verdict[0].text,
			Agreement:      verdict[1].text,
			Disagreement:   verdict[2].text,
			Minority:       verdict[3].text,
		}
	case o.State == CouncilCompleted:
		r.VerdictUnavailable = v.Reason
	}
	for _, s := range o.Standings {
		r.Ranking = append(r.Ranking, resultRank{Member: s.Member, Placement: s.Placement, Score: s.Score, Ballots: s.Ballots})
	}
	for _, a := range received {
		r.Answers = append(r.Answers, resultAnswer{Member: a.Member, Body: a.Body})
	}
	for _, a := range f.absences() {
		r.Absent = append(r.Absent, resultAbsent{Member: a.Member, Stage: a.Stage, Reason: a.Reason})
	}
	for _, x := range f.Excluded {
		r.Excluded = append(r.Excluded, resultMember{Member: x.Name, Reason: x.Reason})
	}
	return r
}

// receivedAnswerBudget caps the answers a failed council's reply carries,
// once in the text and once in the result block. JSON can escape a byte to
// six, so both copies stay under the relay's 256 KB body limit; the report
// keeps every answer in full.
const receivedAnswerBudget = 32 << 10

// receivedAnswers returns the answers cut to share receivedAnswerBudget.
func receivedAnswers(answers []Answer) []Answer {
	bodies, _ := truncateAll(answers, fit(lengths(answers), receivedAnswerBudget))
	out := make([]Answer, len(answers))
	for i, a := range answers {
		a.Body = bodies[i]
		out[i] = a
	}
	return out
}

func (a Answer) parts() (string, string) { return a.Member, a.Body }

// verdictBudget caps the chairman's verdict sections a completed council's
// reply carries, once in the text and once in the result block, for the
// same reason and by the same arithmetic as receivedAnswerBudget; the
// report keeps the verdict in full.
const verdictBudget = 32 << 10

// verdictSection is one section of the chairman's verdict, authors named.
type verdictSection struct{ head, text string }

func (s verdictSection) parts() (string, string) { return s.head, s.text }

// verdictSections returns the recommendation, agreement, disagreement,
// and minority sections with the authors named, cut to share
// verdictBudget.
func (f Finished) verdictSections() []verdictSection {
	v := f.Outcome.Verdict
	secs := []verdictSection{
		{"Recommendation", revealLabels(v.Recommendation, v.Labels)},
		{"Agreement", revealLabels(v.Agreement, v.Labels)},
		{"Disagreement", revealLabels(v.Disagreement, v.Labels)},
		{"Minority", revealLabels(v.Minority, v.Labels)},
	}
	texts, _ := truncateAll(secs, fit(lengths(secs), verdictBudget))
	for i := range secs {
		secs[i].text = texts[i]
	}
	return secs
}
