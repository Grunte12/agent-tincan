package council

import (
	"bytes"
	"fmt"
	"image/png"
	"strings"
	"testing"
	"time"
)

// decodeCard checks that b is a 1600x900 PNG.
func decodeCard(t *testing.T, b []byte) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("card does not decode as PNG: %v", err)
	}
	if got := img.Bounds().Size(); got.X != 1600 || got.Y != 900 {
		t.Fatalf("card is %dx%d, want 1600x900", got.X, got.Y)
	}
}

func TestScorecardLongQuestion(t *testing.T) {
	f := completedCouncil()
	f.Question = strings.Repeat("How should the relay shard its data? ", 14)[:500]
	var buf bytes.Buffer
	if err := DrawScorecard(&buf, f); err != nil {
		t.Fatal(err)
	}
	decodeCard(t, buf.Bytes())
}

func TestScorecardEmojiQuestion(t *testing.T) {
	f := completedCouncil()
	f.Question = "Ship it today? 🚀🔥 or wait 👩‍💻 日本語"
	var buf bytes.Buffer
	if err := DrawScorecard(&buf, f); err != nil {
		t.Fatal(err)
	}
	decodeCard(t, buf.Bytes())

	got := cardText("Ship it 🚀?")
	if strings.ContainsRune(got, '🚀') || !strings.ContainsRune(got, replacementRune()) {
		t.Errorf("cardText = %q, want the rocket replaced by %q", got, replacementRune())
	}
}

func TestScorecardVerdictUnavailable(t *testing.T) {
	f := completedCouncil()
	f.Outcome.Verdict = Verdict{Category: Uncategorized, Unavailable: true, Reason: "no chairman"}
	if got := verdictLine(f); !strings.Contains(got, "Verdict unavailable") {
		t.Errorf("verdict line = %q, want it to say the verdict is unavailable", got)
	}
	var buf bytes.Buffer
	if err := DrawScorecard(&buf, f); err != nil {
		t.Fatal(err)
	}
	decodeCard(t, buf.Bytes())
}

func TestLeaderboardCard(t *testing.T) {
	at := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	for _, n := range []int{1, 10} {
		var rows []LeaderboardRow
		for i := range n {
			rows = append(rows, LeaderboardRow{Member: fmt.Sprintf("member-%d-web", i), Councils: 12 - i, MeanScore: 1 - float64(i)/10, Wins: n - i})
		}
		var buf bytes.Buffer
		if err := DrawLeaderboard(&buf, rows, "architecture", at); err != nil {
			t.Fatalf("%d rows: %v", n, err)
		}
		decodeCard(t, buf.Bytes())
	}
}
