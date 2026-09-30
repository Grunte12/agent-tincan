package council

import (
	"slices"
	"strings"
	"testing"
)

func TestFreeTextIsTheQuestion(t *testing.T) {
	r, err := ParseRequest("  Should we shard the store?\nWe have 3 nodes.  ", DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if r.Op != OpConvene || r.Question != "Should we shard the store?\nWe have 3 nodes." ||
		r.Members != nil || r.Exclude != nil || r.Chairman != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestParseRequestForms(t *testing.T) {
	for body, want := range map[string]Request{
		`council: {"question":"Which cache?","members":["codex","grok-web"],"chairman":"grok-web"}`: {Op: OpConvene, Question: "Which cache?", Members: []string{"codex", "grok-web"}, Chairman: "grok-web"},
		"COUNCIL:\n{\"question\":\"Which?\",\"exclude\":[\"hermes\"]}":                              {Op: OpConvene, Question: "Which?", Exclude: []string{"hermes"}},
		`council: {"op":"convene","question":"q"}`:                                                  {Op: OpConvene, Question: "q"},
		`council: {"op":"leaderboard"}`:                                                             {Op: OpLeaderboard},
		`council: {"op":"leaderboard","category":"debugging"}`:                                      {Op: OpLeaderboard, Category: "debugging"},
		`council: {"op":"leaderboard","category":"uncategorized"}`:                                  {Op: OpLeaderboard, Category: "uncategorized"},
	} {
		got, err := ParseRequest(body, DefaultConfig())
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if got.Op != want.Op || got.Question != want.Question || !slices.Equal(got.Members, want.Members) ||
			!slices.Equal(got.Exclude, want.Exclude) || got.Chairman != want.Chairman || got.Category != want.Category {
			t.Fatalf("%q: got %+v, want %+v", body, got, want)
		}
	}
}

func TestMalformedFormDeclinesWithReasonAndShape(t *testing.T) {
	for body, reason := range map[string]string{
		`council: {"question":"q"`:                                           "not the council schema",
		`council: {"question":"q","model":"x"}`:                              "not the council schema",
		`council: {"question":"  "}`:                                         "question is empty",
		`council: {"members":["codex"]}`:                                     "question is required",
		`council: {"question":"q","question":"r"}`:                           "more than once",
		`council: {"question":"q","chairman":null}`:                          "chairman is null",
		`council: {"question":"q"} trailing`:                                 "text after",
		`council: {"op":"vote","question":"q"}`:                              "not convene or leaderboard",
		`council: {"op":"leaderboard","question":"q"}`:                       "question is not a field of leaderboard",
		`council: {"op":"leaderboard","category":"cooking"}`:                 "category",
		`council: {"question":"q","category":"debugging"}`:                   "category is not a field of convene",
		`council: {"question":"q","members":[]}`:                             "members is empty",
		`council: {"question":"q","members":["codex",""]}`:                   "empty name",
		`council: {"question":"q","members":["codex","codex"]}`:              "twice",
		`council: {"question":"q","members":["codex"],"exclude":["hermes"]}`: "members or exclude",
		"council:\nnull": "null",
	} {
		_, err := ParseRequest(body, DefaultConfig())
		if err == nil {
			t.Errorf("%s: parsed", body)
			continue
		}
		if !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), FormHelp) {
			t.Errorf("%s: error %q lacks reason %q or the form's shape", body, err, reason)
		}
	}
}

func TestEmptyFreeTextIsRefused(t *testing.T) {
	if _, err := ParseRequest(" \n ", DefaultConfig()); err == nil {
		t.Fatal("blank body accepted")
	}
}
