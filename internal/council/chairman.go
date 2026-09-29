package council

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Verdict is what the chairman stage produced. The chairman writes it but
// cannot change the peer standings.
type Verdict struct {
	// Chairman is the candidate that wrote the verdict; empty when
	// Unavailable.
	Chairman string
	// Category is one of Config.Categories, or Uncategorized when the
	// chairman named none of them or no verdict came back.
	Category string
	// The verdict's sections, as the chairman wrote them. They refer to
	// answers by the labels in Labels.
	Recommendation, Agreement, Disagreement, Minority string
	// Labels maps each label the chairman saw to the member whose answer
	// it stood for, so authorship can be revealed after judging.
	Labels map[string]string
	// Unavailable is set when no candidate delivered a verdict (R17); the
	// council still completes with its ranking, and Reason says why.
	Unavailable bool
	Reason      string
	// Failed are the candidates tried that gave no verdict, in the order
	// they were tried.
	Failed []Absence
}

// chair runs the chairman stage for a completed outcome: it tries c's
// chairman candidates in turn, idle ones first, until one returns a
// verdict or the stage's time limit passes. Each candidate but the last
// gets half the time left, so a stalled one leaves room for the rest. An
// error is returned only when ctx ends.
func (e *Engine) chair(ctx context.Context, c Council, out Outcome) (Verdict, error) {
	v := Verdict{Category: Uncategorized, Labels: map[string]string{}}
	if len(c.Chairmen) == 0 {
		v.Unavailable, v.Reason = true, "no chairman candidate is eligible"
		return v, nil
	}

	// The chairman sees the answers in tally order under its own labels.
	var order []Answer
	var tally []tallyLine
	labels := e.labels(len(out.Answers))
	for _, s := range out.Standings {
		i := slices.IndexFunc(out.Answers, func(a Answer) bool { return a.Member == s.Member })
		order = append(order, out.Answers[i])
		tally = append(tally, tallyLine{Label: labels[len(tally)], Placement: s.Placement, Score: s.Score, Ballots: s.Ballots})
	}
	for _, a := range out.Answers {
		if !slices.ContainsFunc(order, func(o Answer) bool { return o.Member == a.Member }) {
			order = append(order, a)
			tally = append(tally, tallyLine{Label: labels[len(tally)]})
		}
	}
	shown := make([]labeledAnswer, len(order))
	for i, a := range order {
		shown[i] = labeledAnswer{Label: labels[i], Text: a.Copy}
		v.Labels[labels[i]] = a.Member
	}
	body, _, err := chairmanPrompt(c.Question, tally, shown, e.Config.Categories, randomNonce())
	if err != nil {
		v.Unavailable, v.Reason = true, err.Error()
		return v, nil
	}

	candidates := idleFirst(c.Chairmen, out.Absent)
	deadline := e.Now().Add(e.Config.ChairmanLimit)
	for i, cand := range candidates {
		left := deadline.Sub(e.Now())
		if left <= 0 {
			break
		}
		if i < len(candidates)-1 {
			left /= 2
		}
		c.progress("Chairman %s writing the verdict", cand)
		res, _, err := e.stage(ctx, c.Request.ID, []plan{{member: cand, out: client.Outgoing{To: cand, Body: body}}}, left)
		if err != nil {
			return v, err
		}
		r := res[cand]
		if r.Status != envelope.StatusAnswered {
			v.Failed = append(v.Failed, absence(cand, StageChairman, r))
			continue
		}
		sections, ok := parseVerdict(r.Reply.Body)
		if !ok {
			detail, _ := truncate(strings.TrimSpace(r.Reply.Body), 300)
			v.Failed = append(v.Failed, Absence{Member: cand, Stage: StageChairman, Reason: AbsentNoVerdict, Detail: detail})
			continue
		}
		v.Chairman = cand
		if cat := normalizeCategory(sections[headCategory]); slices.Contains(e.Config.Categories, cat) {
			v.Category = cat
		}
		v.Recommendation = sections[headRecommendation]
		v.Agreement = sections[headAgreement]
		v.Disagreement = sections[headDisagreement]
		v.Minority = sections[headMinority]
		return v, nil
	}
	v.Unavailable = true
	v.Reason = fmt.Sprintf("no chairman candidate delivered a verdict within %v", e.Config.ChairmanLimit)
	return v, nil
}

// idleFirst orders candidates with those absent from an earlier stage last:
// their site may still be busy with that stage's ask, while a candidate
// whose ballot came back, or that did not sit, is idle.
func idleFirst(candidates []string, absent []Absence) []string {
	busy := func(name string) bool {
		return slices.ContainsFunc(absent, func(a Absence) bool { return a.Member == name && a.Reason != AbsentUnranked })
	}
	out := slices.DeleteFunc(slices.Clone(candidates), busy)
	for _, c := range candidates {
		if busy(c) {
			out = append(out, c)
		}
	}
	return out
}

// Verdict section headers, in the order the chairman writes them.
const (
	headCategory       = "CATEGORY"
	headRecommendation = "RECOMMENDATION"
	headAgreement      = "AGREEMENT"
	headDisagreement   = "DISAGREEMENT"
	headMinority       = "MINORITY"
)

// verdictHeader is a section header line: "CATEGORY: x", "**Agreement:**
// x", or "## Minority" alone on its line.
var verdictHeader = regexp.MustCompile(`(?i)^[\s>#*_]*(CATEGORY|RECOMMENDATION|AGREEMENT|DISAGREEMENT|MINORITY)[\s*_]*(:.*)?$`)

// parseVerdict reads the chairman's sections from the reply's last verdict
// block, which starts at its last CATEGORY header (or, without one, its
// last RECOMMENDATION header), so a block quoted from an answer earlier in
// the reply cannot stand in for it. Nothing else in the reply is read. ok
// is false when the block has no recommendation.
func parseVerdict(reply string) (map[string]string, bool) {
	lines := strings.Split(reply, "\n")
	heads := make([]string, len(lines))
	start := -1
	for i, l := range lines {
		if m := verdictHeader.FindStringSubmatch(l); m != nil {
			heads[i] = strings.ToUpper(m[1])
		}
	}
	for _, want := range []string{headCategory, headRecommendation} {
		for i := len(heads) - 1; i >= 0 && start < 0; i-- {
			if heads[i] == want {
				start = i
			}
		}
	}
	if start < 0 {
		return nil, false
	}
	sections := map[string]string{}
	var cur string
	var buf []string
	flush := func() {
		if cur != "" {
			sections[cur] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
	}
	for i := start; i < len(lines); i++ {
		if heads[i] == "" {
			buf = append(buf, lines[i])
			continue
		}
		flush()
		cur = heads[i]
		rest := verdictHeader.FindStringSubmatch(lines[i])[2]
		buf = []string{strings.Trim(strings.TrimPrefix(rest, ":"), " \t*_")}
	}
	flush()
	return sections, sections[headRecommendation] != ""
}

// normalizeCategory turns the chairman's category text into the form of
// Config.Categories: lowercase words joined by hyphens.
func normalizeCategory(s string) string {
	s = strings.ToLower(strings.Trim(s, " \t*_`'\".[]"))
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '_' || r == '-' }), "-")
}
