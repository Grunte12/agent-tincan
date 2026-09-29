package council

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

// rankingHeader starts a reviewer's ballot. Only the last one in a reply
// counts, so a header quoted from an answer cannot stand in for it.
var rankingHeader = regexp.MustCompile(`(?i)FINAL\s+RANKING\s*:`)

var (
	// rankedItem is one numbered line: "1. ...", "2) ...".
	rankedItem = regexp.MustCompile(`^\s*\**\s*\d+\s*[.):]\s*(.*)$`)
	// namedLabel is "Answer K" anywhere in an item.
	namedLabel = regexp.MustCompile(`\b(?i:answer|response)\s+\**([A-Z]{1,2})\b`)
	// bareLabel is an item that starts with the label alone: "K - solid".
	bareLabel = regexp.MustCompile(`^\**([A-Z]{1,2})\**(?:$|[\s.):,*-])`)
)

// parseBallot reads a reviewer's ranking, best first, as labels. It reads
// the numbered list after the reply's last "FINAL RANKING:" header or,
// when there is no header, its last numbered list naming answers (the one
// lenient fallback). Only labels in issued count, each once. It returns
// nil when no ballot can be read.
func parseBallot(reply string, issued []string) []string {
	if locs := rankingHeader.FindAllStringIndex(reply, -1); len(locs) > 0 {
		return rankedLabels(strings.Split(reply[locs[len(locs)-1][1]:], "\n"), issued, true)
	}
	lines := strings.Split(reply, "\n")
	// The last run of numbered lines that names at least one label.
	var got []string
	for i := 0; i < len(lines); i++ {
		if !rankedItem.MatchString(lines[i]) {
			continue
		}
		j := i
		for j < len(lines) && rankedItem.MatchString(lines[j]) {
			j++
		}
		if r := rankedLabels(lines[i:j], issued, false); r != nil {
			got = r
		}
		i = j
	}
	return got
}

// rankedLabels reads labels from the numbered lines at the start of lines,
// skipping blank lines before the first one and stopping at the first
// other line. bare also accepts an item that is the label alone.
func rankedLabels(lines []string, issued []string, bare bool) []string {
	var out []string
	started := false
	for _, line := range lines {
		m := rankedItem.FindStringSubmatch(line)
		if m == nil {
			if !started && strings.TrimSpace(strings.Trim(line, "*")) == "" {
				continue
			}
			break
		}
		started = true
		label := ""
		if n := namedLabel.FindStringSubmatch(m[1]); n != nil {
			label = n[1]
		} else if b := bareLabel.FindStringSubmatch(strings.TrimSpace(m[1])); bare && b != nil {
			label = b[1]
		}
		if label != "" && slices.Contains(issued, label) && !slices.Contains(out, label) {
			out = append(out, label)
		}
	}
	return out
}

// Standing is one member's peer score in a council.
type Standing struct {
	Member string
	// Score is the mean over ballots of the member's normalized Borda
	// points: 1 for first place on a ballot, 0 for last.
	Score float64
	// Ballots counts the valid ballots that ranked the member.
	Ballots int
	// Placement is 1 for the top score; equal scores share a placement.
	Placement int
}

// minBallotSize is the fewest answers a ballot must rank, once the
// reviewer's own is dropped, to say anything.
const minBallotSize = 2

// score tallies the reviews' ballots. Each ballot drops the reviewer's
// vote on its own answer, needs at least minBallotSize answers left to
// count, and gives each answer it ranked (n-1-i)/(n-1) points at position
// i of n. A member's score is the mean over the ballots that ranked it.
// It returns the standings, best first, and how many ballots counted.
func score(reviews []Review) ([]Standing, int) {
	sum := map[string]float64{}
	count := map[string]int{}
	valid := 0
	for _, r := range reviews {
		ranked := countedBallot(r)
		if len(ranked) < minBallotSize {
			continue
		}
		valid++
		n := float64(len(ranked) - 1)
		for i, m := range ranked {
			sum[m] += (n - float64(i)) / n
			count[m]++
		}
	}
	out := make([]Standing, 0, len(count))
	for m, c := range count {
		out = append(out, Standing{Member: m, Score: sum[m] / float64(c), Ballots: c})
	}
	slices.SortFunc(out, func(a, b Standing) int {
		if !sameScore(a.Score, b.Score) {
			return cmp.Compare(b.Score, a.Score)
		}
		return cmp.Compare(a.Member, b.Member)
	})
	for i := range out {
		out[i].Placement = i + 1
		if i > 0 && sameScore(out[i].Score, out[i-1].Score) {
			out[i].Placement = out[i-1].Placement
		}
	}
	return out, valid
}

// countedBallot is r's ballot without the reviewer's own answer.
func countedBallot(r Review) []string {
	return slices.DeleteFunc(slices.Clone(r.Ranked), func(m string) bool { return m == r.Reviewer })
}

func sameScore(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
