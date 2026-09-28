package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func perplexityFixture(t *testing.T, slug string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "perplexity", "thread-"+slug+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A thread read parses into turns: the question, the ask_text answer with
// its [n] markers, and the web sources numbered by their place in the
// site's list (images and memories left out).
func TestPerplexityThreadParses(t *testing.T) {
	th, err := parsePerplexityDetail("x", perplexityFixture(t, "0e1d0000-0000-4000-8000-0000000000a1"))
	if err != nil {
		t.Fatal(err)
	}
	if th.conv.ID != "0e1d0000-0000-4000-8000-0000000000a1" || th.conv.Title != "Tin can telephones" || th.conv.Source != SourcePerplexity {
		t.Fatalf("conv %+v", th.conv)
	}
	if len(th.turns) != 2 {
		t.Fatalf("%d turns", len(th.turns))
	}
	t0 := th.turns[0]
	if t0.promptID != "0e1d0000-0000-4000-8000-0000000000d1" || t0.prompt.Text != "What is a tin can telephone?" {
		t.Fatalf("turn 0 prompt %+v %q", t0.prompt, t0.promptID)
	}
	if t0.reply.Text != "A tin can telephone is two cans joined by a string [1][3]." {
		t.Fatalf("turn 0 reply %q", t0.reply.Text)
	}
	if want := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC); !t0.prompt.Time.Equal(want) {
		t.Fatalf("turn 0 time %s", t0.prompt.Time)
	}
	want := []webSource{
		{n: 1, title: "Example page one", url: "https://example.com/one"},
		{n: 3, title: "Example page two", url: "https://example.org/two"},
		{n: 4, title: "Example page one again", url: "https://example.com/one"},
	}
	if fmt.Sprint(t0.replySources) != fmt.Sprint(want) {
		t.Fatalf("sources %+v", t0.replySources)
	}
	if th.turns[1].reply.Text != "Tens of meters, if it is kept taut [1]." || len(th.turns[1].replySources) != 1 {
		t.Fatalf("turn 1 %+v", th.turns[1])
	}
	if !th.conv.UpdatedAt.Equal(time.Date(2026, 9, 20, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("updated %s", th.conv.UpdatedAt)
	}
}

// The reply wait sees each entry as a user node and a reply node; the
// reply is finished only when the entry is COMPLETED and its markdown DONE.
func TestPerplexityNodesFinishedMarker(t *testing.T) {
	nodes, err := perplexityNodes(perplexityFixture(t, "0e1d0000-0000-4000-8000-0000000000a1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 4 || !nodes[0].user || !nodes[1].reply || !nodes[1].finished || !nodes[1].endTurn || nodes[0].id != "0e1d0000-0000-4000-8000-0000000000d1" {
		t.Fatalf("nodes %+v", nodes)
	}
	nodes, err = perplexityNodes(perplexityFixture(t, "0e1d0000-0000-4000-8000-0000000000a2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[1].finished || nodes[1].endTurn || nodes[1].text != "Half an ans" {
		t.Fatalf("pending nodes %+v", nodes)
	}
	// COMPLETED with the markdown still streaming is not finished yet.
	raw := `{"entries":[{"uuid":"e-1","status":"COMPLETED","query_str":"q","blocks":[{"intended_usage":"ask_text","markdown_block":{"progress":"IN_PROGRESS","answer":"part"}}]}]}`
	nodes, err = perplexityNodes(json.RawMessage(raw))
	if err != nil || nodes[1].finished {
		t.Fatalf("%+v %v", nodes, err)
	}
	// A failed entry ends the turn with no answer.
	raw = `{"entries":[{"uuid":"e-1","status":"FAILED","query_str":"q","blocks":[]}]}`
	nodes, err = perplexityNodes(json.RawMessage(raw))
	if err != nil || nodes[1].finished || !nodes[1].endTurn {
		t.Fatalf("%+v %v", nodes, err)
	}
}

// Older shapes are read as fallbacks: a workflow block's answer items, or
// the entry text's FINAL step with its own web results.
func TestPerplexityFallbackShapes(t *testing.T) {
	wf := `{"entries":[{"uuid":"e-1","status":"COMPLETED","query_str":"q","blocks":[{"intended_usage":"workflow_root","workflow_block":{"steps":[{"items":[{"payload":{"text_payload":{"text":"","chunks":["from ","workflow"],"variant":"answer"}}},{"payload":{"text_payload":{"text":"a thought","variant":"thinking"}}}]}]}}]}]}`
	th, err := parsePerplexityDetail("s-1", json.RawMessage(wf))
	if err != nil || th.turns[0].reply.Text != "from workflow" || th.conv.ID != "s-1" {
		t.Fatalf("workflow: %+v %v", th, err)
	}
	final, _ := json.Marshal(`{"answer":"from final","web_results":[{"name":"Final source","url":"https://example.com/final"}]}`)
	steps, _ := json.Marshal(`[{"step_type":"INITIAL_QUERY","content":{}},{"step_type":"FINAL","content":{"answer":` + string(final) + `}}]`)
	txt := `{"entries":[{"backend_uuid":"b-1","status":"COMPLETED","query_str":"q","text":` + string(steps) + `,"blocks":[]}]}`
	th, err = parsePerplexityDetail("s-1", json.RawMessage(txt))
	if err != nil || th.turns[0].reply.Text != "from final" || th.turns[0].promptID != "b-1" || len(th.turns[0].replySources) != 1 || th.turns[0].replySources[0].url != "https://example.com/final" {
		t.Fatalf("final: %+v %v", th, err)
	}
	plain := `{"entries":[{"uuid":"e-1","status":"COMPLETED","query_str":"q","text":"plain answer","blocks":[]}]}`
	if th, err = parsePerplexityDetail("s-1", json.RawMessage(plain)); err != nil || th.turns[0].reply.Text != "plain answer" {
		t.Fatalf("plain: %+v %v", th, err)
	}
}

// Any unexpected shape is an error (endpoint_changed to the caller),
// never an empty answer taken as real.
func TestPerplexityUnexpectedShapes(t *testing.T) {
	for name, raw := range map[string]string{
		"not an object":         `[1,2]`,
		"no entries":            `{"status":"success"}`,
		"entries not a list":    `{"entries":{}}`,
		"entry without an id":   `{"entries":[{"status":"COMPLETED","query_str":"q"}]}`,
		"entry id bad":          `{"entries":[{"uuid":"../x","status":"COMPLETED","query_str":"q"}]}`,
		"status not a string":   `{"entries":[{"uuid":"e-1","status":3,"query_str":"q"}]}`,
		"blocks not a list":     `{"entries":[{"uuid":"e-1","status":"COMPLETED","query_str":"q","blocks":{}}]}`,
		"finished, no answer":   `{"entries":[{"uuid":"e-1","status":"COMPLETED","query_str":"q","blocks":[{"intended_usage":"something_new","x_block":{}}]}]}`,
		"finished, JSON text":   `{"entries":[{"uuid":"e-1","status":"COMPLETED","query_str":"q","text":"[{\"step_type\":\"OTHER\"}]","blocks":[]}]}`,
		"query not a string":    `{"entries":[{"uuid":"e-1","status":"PENDING","query_str":7}]}`,
		"entries hold a string": `{"entries":["e-1"]}`,
	} {
		if _, err := parsePerplexityDetail("s-1", json.RawMessage(raw)); err == nil {
			t.Errorf("%s: parsed", name)
		}
		if _, err := perplexityNodes(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: nodes parsed", name)
		}
	}
	// A block of an unknown kind, or a known block whose fields changed
	// type, is skipped rather than failing the read.
	raw := `{"entries":[{"uuid":"e-1","status":"COMPLETED","query_str":"q","blocks":[{"intended_usage":"web_results","web_result_block":{"web_results":"nope"}},{"intended_usage":"ask_text","markdown_block":{"progress":"DONE","answer":"ok"}}]}]}`
	if th, err := parsePerplexityDetail("s-1", json.RawMessage(raw)); err != nil || th.turns[0].reply.Text != "ok" || len(th.turns[0].replySources) != 0 {
		t.Fatalf("%+v %v", th, err)
	}
	// An unparseable time is unknown, not an error.
	raw = `{"entries":[{"uuid":"e-1","status":"PENDING","query_str":"q","entry_created_datetime":"yesterday"}]}`
	if th, err := parsePerplexityDetail("s-1", json.RawMessage(raw)); err != nil || !th.turns[0].prompt.Time.IsZero() {
		t.Fatalf("%+v %v", th, err)
	}
}

// Perplexity is read only by thread; it has no history list.
func TestPerplexityReaderHasNoList(t *testing.T) {
	r := NewPerplexity(&Client{})
	if _, err := r.List(t.Context(), 1, Options{}); err == nil {
		t.Fatal("list worked")
	}
	if _, err := r.Read(t.Context(), Query{Source: SourcePerplexity, Mode: ModeLatest}, Options{}); err == nil {
		t.Fatal("latest worked")
	}
}

// The sources footer: one line per source in the site's order, each URL
// once, only http(s), titles on one line, capped at maxReplySources with
// the rest counted.
func TestSourcesFooter(t *testing.T) {
	if got := sourcesFooter(nil); got != "" {
		t.Fatalf("empty: %q", got)
	}
	got := sourcesFooter([]webSource{
		{n: 1, title: "One", url: "https://example.com/one"},
		{n: 2, title: "Dup", url: "https://example.com/one"},
		{n: 3, title: "  Two\nlines\there ", url: "http://example.org/two"},
		{n: 4, title: "Script", url: "javascript:alert(1)"},
		{n: 5, title: "", url: "https://example.net/untitled"},
		{title: "No number", url: "https://example.net/plain"},
		{n: 7, title: "Relative", url: "/relative"},
	})
	want := "Sources:\n- [1] One https://example.com/one\n- [3] Two lines here http://example.org/two\n- [5] https://example.net/untitled\n- No number https://example.net/plain"
	if got != want {
		t.Fatalf("footer:\n%s\nwant:\n%s", got, want)
	}
	var many []webSource
	for i := range 14 {
		many = append(many, webSource{n: i + 1, title: fmt.Sprintf("S%d", i+1), url: fmt.Sprintf("https://example.com/%d", i+1)})
	}
	many = append(many, webSource{n: 15, title: "dup of 1", url: "https://example.com/1"})
	got = sourcesFooter(many)
	lines := strings.Split(got, "\n")
	if len(lines) != 12 || lines[10] != "- [10] S10 https://example.com/10" || lines[11] != "(and 4 more)" {
		t.Fatalf("capped footer:\n%s", got)
	}
	long := sourcesFooter([]webSource{{title: strings.Repeat("é", 300), url: "https://example.com/x"}})
	if len(long) > len("Sources:\n- ")+maxSourceTitle+len(" https://example.com/x") {
		t.Fatalf("title not capped: %d bytes", len(long))
	}
}
