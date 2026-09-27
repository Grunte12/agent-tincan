package history

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

const (
	grokConv1 = "0e1d0000-0000-4000-8000-000000000001"
	grokConv2 = "0e1d0000-0000-4000-8000-000000000002"
)

// grokDetailFixture builds the extension's grok.detail result from the
// two recorded answers it combines: response-node's tree and in-flight
// list, and load-responses' bodies.
func grokDetailFixture(t *testing.T, id string) (json.RawMessage, bool) {
	t.Helper()
	nb, err := os.ReadFile("testdata/grok/response-node-" + id + ".json")
	if err != nil {
		return nil, false
	}
	var nodes struct {
		ResponseNodes     json.RawMessage `json:"responseNodes"`
		InflightResponses json.RawMessage `json:"inflightResponses"`
	}
	var loaded struct {
		Responses json.RawMessage `json:"responses"`
	}
	if err := json.Unmarshal(nb, &nodes); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture(t, "grok/load-responses-"+id+".json"), &loaded); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"conversationId": id, "responseNodes": nodes.ResponseNodes, "inflightResponses": nodes.InflightResponses, "responses": loaded.Responses})
	return b, true
}

func grokFake(t *testing.T) *fakeChannel {
	return &fakeChannel{handle: func(req NativeRequest) ([]NativeResponse, error) {
		if err := ValidateOp(req.Op, req.Args); err != nil {
			return []NativeResponse{{Error: &NativeError{Code: "bad_request", Message: err.Error()}}}, nil
		}
		switch req.Op {
		case OpGrokList:
			return []NativeResponse{{OK: true, Result: fixture(t, "grok/conversations.json")}}, nil
		case OpGrokDetail:
			b, ok := grokDetailFixture(t, req.Args.ID)
			if !ok {
				return []NativeResponse{{Error: &NativeError{Code: "not_found", Message: "404"}}}, nil
			}
			return []NativeResponse{{OK: true, Result: b}}, nil
		case OpGrokFile:
			return chunkFrames(append(fakePNG(64), req.Args.FileID...), "image/jpeg", 50), nil
		}
		return nil, errors.New("unexpected op")
	}}
}

func newTestGrok(ch Channel) *Grok {
	r := NewGrok(&Client{Channel: ch, Timeout: 2 * time.Second, Cooldown: &SiteCooldown{}})
	r.Now = func() time.Time { return liveNow }
	r.AgentChats = ""
	return r
}

func TestGrokList(t *testing.T) {
	r := newTestGrok(grokFake(t))
	convs, err := convsOf(r.List(context.Background(), 10, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 2 || convs[0].Title != "Fox mascot sketch" || convs[1].Title != "Weekend hike ideas" || convs[0].Source != SourceGrok {
		t.Fatalf("list %+v (the ancient chat is outside the window)", convs)
	}
	if !convs[0].UpdatedAt.Equal(time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("updated %v", convs[0].UpdatedAt)
	}
}

// The latest turn is on the current branch (the newest response, not the
// regenerated draft listed after it), with its generated image fetched by
// response id and index, in its own conversation.
func TestGrokLatestWithImages(t *testing.T) {
	fake := grokFake(t)
	r := newTestGrok(fake)
	convs, err := convsOf(r.Read(context.Background(), Query{Source: SourceGrok, Mode: ModeLatest, WantImages: true}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 || len(convs[0].Messages) != 2 || convs[0].Title != "Fox mascot sketch" {
		t.Fatalf("latest %+v", convs)
	}
	u, a := convs[0].Messages[0], convs[0].Messages[1]
	if u.Text != "draw Tin as a fox in a tin can" || !u.Time.Equal(time.Date(2026, 9, 22, 10, 59, 0, 0, time.UTC)) {
		t.Fatalf("prompt %+v", u)
	}
	if a.Text != "Here is Tin, peeking out of a tin can." {
		t.Fatalf("reply %q (the regenerated draft must not win)", a.Text)
	}
	if got := imageSHAs(convs[0], RoleAssistant); len(got) != 1 || got[0] != "5e5f0000-0000-4000-8000-000000000015_0" {
		t.Fatalf("reply image %v", got)
	}
	if a.Images[0].Name != "image.jpg" {
		t.Fatalf("image name %q", a.Images[0].Name)
	}
	var file *NativeRequest
	for i, c := range fake.calls {
		if c.Op == OpGrokFile {
			file = &fake.calls[i]
		}
	}
	if file == nil || file.Args.ConversationID != grokConv1 {
		t.Fatalf("file op %+v", file)
	}
}

func TestGrokSearchConversationAndNotFound(t *testing.T) {
	r := newTestGrok(grokFake(t))
	convs, err := convsOf(r.Read(context.Background(), Query{Source: SourceGrok, Mode: ModeSearch, Terms: []string{"Portland"}}, Options{}))
	if err != nil || len(convs) != 1 || convs[0].ID != grokConv2 {
		t.Fatalf("search %+v %v", convs, err)
	}
	if convs[0].Messages[1].Text != "Try the dummy falls loop." {
		t.Fatalf("messages %+v", convs[0].Messages)
	}
	convs, err = convsOf(r.Read(context.Background(), Query{Source: SourceGrok, Mode: ModeConversation, ConversationID: grokConv1}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range convs[0].Messages {
		texts = append(texts, m.Text)
	}
	if got := strings.Join(texts, "|"); got != "name three fox mascots|Rusty, Ember and Tin.|draw Tin as a fox in a tin can|Here is Tin, peeking out of a tin can." {
		t.Fatalf("conversation %q", got)
	}
	_, err = convsOf(r.Read(context.Background(), Query{Source: SourceGrok, Mode: ModeConversation, ConversationID: "0e1d0000-0000-4000-8000-00000000dead"}, Options{}))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := convsOf(r.Read(context.Background(), Query{Source: SourceChatGPT, Mode: ModeLatest}, Options{})); err == nil {
		t.Fatal("grok reader answered a chatgpt query")
	}
}

// Conversations grok-web sent into are left out of the owner's history
// unless all is asked for, and then marked automated.
func TestGrokHistoryLeavesOutWebAgentChats(t *testing.T) {
	r := newTestGrok(grokFake(t))
	r.AgentChats = filepath.Join(t.TempDir(), "web-agent-grok-conversations.json")
	if err := recordWebUsed(r.AgentChats, grokConv1, liveNow); err != nil {
		t.Fatal(err)
	}
	convs, err := convsOf(r.Read(context.Background(), Query{Source: SourceGrok, Mode: ModeLatest}, Options{}))
	if err != nil || len(convs) != 1 || convs[0].ID != grokConv2 || convs[0].Messages[0].Text != "suggest a short hike near Portland" {
		t.Fatalf("latest without the agent's chat: %+v %v", convs, err)
	}
	convs, err = convsOf(r.Read(context.Background(), Query{Source: SourceGrok, Mode: ModeLatest}, Options{All: true}))
	if err != nil || len(convs) != 1 || convs[0].ID != grokConv1 || !convs[0].Automated {
		t.Fatalf("latest with all: %+v %v", convs, err)
	}
	list, err := convsOf(r.List(context.Background(), 5, Options{}))
	if err != nil || len(list) != 1 || list[0].ID != grokConv2 {
		t.Fatalf("list %+v %v", list, err)
	}
}

// "what did I last ask Grok" through the history service: the structured
// query reaches the grok reader and the reply names Grok.
func TestServeAnswersAboutGrok(t *testing.T) {
	rig := newServeRig(t, true)
	g := newTestGrok(grokFake(t))
	g.AgentChats = filepath.Join(t.TempDir(), "web-agent-grok-conversations.json")
	if err := recordWebUsed(g.AgentChats, grokConv1, liveNow); err != nil {
		t.Fatal(err)
	}
	rig.svc.Readers[SourceGrok] = g
	res := rig.ask(t, "grokbot", `query: {"source":"grok","mode":"latest"}`)
	if res.Status != envelope.StatusAnswered || !strings.Contains(res.Reply.Body, "suggest a short hike near Portland") || strings.Contains(res.Reply.Body, "fox") {
		t.Fatalf("%s %q", res.Status, res.Reply.Body)
	}
	if !strings.Contains(res.Reply.Body, "Grok") {
		t.Fatalf("reply does not name Grok: %q", res.Reply.Body)
	}
}

func TestGrokFileArgs(t *testing.T) {
	op, a, ok := grokFileArgs(grokConv1, "grok-image://5e5f0000-0000-4000-8000-000000000015_2")
	if !ok || op != OpGrokFile || a.FileID != "5e5f0000-0000-4000-8000-000000000015_2" || a.ConversationID != grokConv1 {
		t.Fatalf("%s %+v %v", op, a, ok)
	}
	if err := ValidateOp(op, a); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct{ conv, pointer string }{
		{grokConv1, "claudeai-file://x"},
		{grokConv1, "grok-image://5e5f_x"},
		{grokConv1, "grok-image://5e5f_01"},
		{grokConv1, "grok-image://5e5f_99"},
		{grokConv1, "grok-image://5e5f"},
		{grokConv1, "grok-image://../x_0"},
		{"", "grok-image://5e5f_0"},
		{"a/b", "grok-image://5e5f_0"},
	} {
		if _, _, ok := grokFileArgs(bad.conv, bad.pointer); ok {
			t.Errorf("%q in %q accepted", bad.pointer, bad.conv)
		}
	}
}

// The reply wait's completion rule for grok.com: an assistant response is
// finished only when partial is false and nothing is in flight.
func TestGrokNodesFinishedMarker(t *testing.T) {
	detail := func(partial bool, inflight string, streamErr string) json.RawMessage {
		errs := "[]"
		if streamErr != "" {
			errs = streamErr
		}
		return json.RawMessage(`{"conversationId":"c1","responseNodes":[{"responseId":"h1","sender":"human"},{"responseId":"a1","sender":"assistant","parentResponseId":"h1"}],"inflightResponses":` + inflight + `,"responses":[` +
			`{"responseId":"h1","message":"hi","sender":"human","createTime":"2026-09-22T10:00:00Z","partial":false},` +
			`{"responseId":"a1","message":"hello","sender":"assistant","createTime":"2026-09-22T10:00:01Z","parentResponseId":"h1","partial":` + map[bool]string{true: "true", false: "false"}[partial] + `,"streamErrors":` + errs + `}]}`)
	}
	for _, c := range []struct {
		partial  bool
		inflight string
		finished bool
	}{
		{true, `[{"responseId":"a1"}]`, false},
		{true, `[]`, false},
		{false, `[{"responseId":"a1"}]`, false},
		{false, `[]`, true},
	} {
		nodes, err := grokNodes(detail(c.partial, c.inflight, ""))
		if err != nil || len(nodes) != 2 || !nodes[0].user || !nodes[1].reply {
			t.Fatalf("%+v %v", nodes, err)
		}
		if nodes[1].finished != c.finished {
			t.Errorf("partial %v inflight %s: finished %v", c.partial, c.inflight, nodes[1].finished)
		}
	}
	nodes, _ := grokNodes(detail(false, "[]", `[{"code":8,"message":"Too many requests"}]`))
	if !nodes[1].limited {
		t.Fatal("a rate-limit stream error is not marked limited")
	}
	nodes, _ = grokNodes(detail(false, "[]", `[{"code":13,"message":"internal"}]`))
	if nodes[1].limited {
		t.Fatal("another stream error is marked limited")
	}
	for _, bad := range []string{`{}`, `{"responseNodes":[]}`, `[]`} {
		if _, err := grokNodes(json.RawMessage(bad)); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}
