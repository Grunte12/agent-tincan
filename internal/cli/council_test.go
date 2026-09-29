package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/council"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/testrelay"
)

// councilEnv is Council's machine: its client config joined to a mesh as
// "council" through a kindless invite, and its own folder.
type councilEnv struct {
	mesh        *testrelay.Mesh
	config, dir string
}

func newCouncilEnv(t *testing.T) councilEnv {
	t.Helper()
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
	m := testrelay.New(t, relay.Config{})
	m.JoinOnMachineOf(t, "muse", "council")
	e := councilEnv{mesh: m, config: filepath.Join(t.TempDir(), "council.json"), dir: filepath.Join(t.TempDir(), "tincan-council")}
	b, _ := json.Marshal(client.Config{Relay: m.URL("council"), Agent: "council"})
	if err := os.WriteFile(e.config, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e councilEnv) setKind(t *testing.T, kind string) {
	t.Helper()
	if err := e.mesh.Client(t, "admin").SetKind(t.Context(), "council", kind); err != nil {
		t.Fatal(err)
	}
}

// council serve refuses to run when the relay does not store kind council
// for it, saying why and how to fix it.
func TestCouncilServeRefusesWithoutCouncilKind(t *testing.T) {
	e := newCouncilEnv(t)
	_, _, err := runSplit(t, councilCmd(), "serve", "--config", e.config, "--dir", e.dir)
	if err == nil || !strings.Contains(err.Error(), "refuses to run") || !strings.Contains(err.Error(), "tincan kind council council") {
		t.Fatalf("serve = %v, want a refusal naming the kind fix", err)
	}
}

// doctor fails the relay kind check until the relay stores kind council,
// and passes once it does.
func TestCouncilDoctorChecksRelayKind(t *testing.T) {
	e := newCouncilEnv(t)
	out, _, err := runSplit(t, councilCmd(), "doctor", "--config", e.config, "--dir", e.dir)
	if err == nil {
		t.Fatalf("doctor passed with no kind:\n%s", out)
	}
	wantLine(t, out, "[FAIL] relay kind", "no kind", "tincan kind council council")

	e.setKind(t, "council")
	out, _, err = runSplit(t, councilCmd(), "doctor", "--config", e.config, "--dir", e.dir)
	if err != nil {
		t.Fatalf("doctor failed with kind council: %v\n%s", err, out)
	}
	wantLine(t, out, "[OK] relay kind", `"council"`)
	wantLine(t, out, "[OK] relay:")
	wantLine(t, out, "[OK] council.json", "using the defaults")
	wantLine(t, out, "[OK] data folder", e.dir)
	wantLine(t, out, "[OK] report folder", filepath.Join(e.dir, "reports"))
}

// A council.json that does not parse fails doctor.
func TestCouncilDoctorRejectsBadSettings(t *testing.T) {
	e := newCouncilEnv(t)
	e.setKind(t, "council")
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "council.json"), []byte(`{"memebers":["x"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runSplit(t, councilCmd(), "doctor", "--config", e.config, "--dir", e.dir)
	if err == nil {
		t.Fatalf("doctor passed with a bad council.json:\n%s", out)
	}
	wantLine(t, out, "[FAIL] council.json", "memebers")
}

// ownerMesh is a mesh with a council-kind agent named councilName on
// muse's machine, and the owner's CLI config joined as owner: "owner"
// joins on the admin device, anything else names a non-admin agent. The
// owner's commands poll every 10ms and see a terminal when tty is set.
func ownerMesh(t *testing.T, councilName, owner string, tty bool) (*testrelay.Mesh, *client.Relay) {
	t.Helper()
	t.Setenv("TINCAN_RELAY", "")
	t.Setenv("TINCAN_PROXY", "")
	// Keep the approve path off any real relay's admin socket.
	t.Setenv("HOME", t.TempDir())
	m := testrelay.New(t, relay.Config{})
	m.Server.SetAttachmentDir(t.TempDir())
	c := m.JoinOnMachineOf(t, "muse", councilName)
	if err := m.Client(t, "admin").SetKind(t.Context(), councilName, "council"); err != nil {
		t.Fatal(err)
	}
	if owner == "owner" {
		m.JoinOnMachineOf(t, "admin", "owner")
	}
	cfg := filepath.Join(t.TempDir(), "owner.json")
	b, _ := json.Marshal(client.Config{Relay: m.URL(owner), Agent: owner})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TINCAN_CONFIG", cfg)
	oldPoll, oldTTY := councilPollEvery, councilInteractive
	councilPollEvery, councilInteractive = 10*time.Millisecond, func() bool { return tty }
	t.Cleanup(func() { councilPollEvery, councilInteractive = oldPoll, oldTTY })
	return m, c
}

// fakeCouncil takes one request as c, posts each note as progress with a
// pause between, and replies with body, status, and the files attached.
// It sends the request it took on the returned channel.
func fakeCouncil(t *testing.T, c *client.Relay, notes []string, body string, status envelope.Status, files ...string) <-chan envelope.Request {
	t.Helper()
	got := make(chan envelope.Request, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			in, err := c.Poll(ctx, time.Second)
			if err != nil || len(in.Requests) == 0 {
				continue
			}
			req := in.Requests[0]
			if _, err := c.Claim(ctx, req.ID); err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			for _, n := range notes {
				if err := c.Progress(ctx, req.ID, n); err != nil {
					t.Errorf("progress: %v", err)
				}
				time.Sleep(200 * time.Millisecond)
			}
			ups, err := c.UploadFiles(ctx, files)
			if err != nil {
				t.Errorf("upload: %v", err)
			}
			if _, err := c.ReplyAttached(ctx, req.ID, body, status, client.AttachmentIDs(ups)); err != nil {
				t.Errorf("reply: %v", err)
			}
			got <- req
			return
		}
	}()
	return got
}

// heldIDs lists the requests waiting for the owner's approval.
func heldIDs(t *testing.T, m *testrelay.Mesh) []string {
	t.Helper()
	var reqs []envelope.Request
	if err := m.Client(t, "admin").Raw(t.Context(), "GET", "/v1/admin/held", nil, &reqs); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range reqs {
		ids = append(ids, r.ID)
	}
	return ids
}

const councilReplyBody = "Recommendation: use Postgres.\nQuestion: Which database?\nStatus: 5 asked, 5 scored, 0 absent, 0 excluded\n\n" +
	"```council-result\n{\n  \"request_id\": \"x\",\n  \"status\": \"answered\",\n  \"state\": \"completed\"\n}\n```\n"

// Run in a terminal on an admin device, tincan council finds the council
// agent by kind, sends the form as the owner, approves its own held
// request, shows each progress note as a stage line in order, prints the
// verdict, and saves the reply's files owner-only.
func TestCouncilConveneOnAdminTerminal(t *testing.T) {
	_, c := ownerMesh(t, "panel", "owner", true)
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.md")
	report := filepath.Join(dir, "report.html")
	card := filepath.Join(dir, "card.png")
	for p, b := range map[string][]byte{plan: []byte("# plan"), report: []byte("<html>report</html>"), card: pngBytes(t)} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	notes := []string{"Answers: 5 of 7 in", "Reviews: 5 of 5 in", "Chairman claude-web writing verdict"}
	got := fakeCouncil(t, c, notes, councilReplyBody, envelope.StatusAnswered, report, card)

	out, errOut, err := runSplit(t, councilCmd(), "Which database?", "--attach", plan, "--members", "claude-web,gemini-web", "--members", "grok-web", "--chairman", "claude-web")
	if err != nil {
		t.Fatalf("council: %v\n%s%s", err, out, errOut)
	}
	req := <-got
	if req.From != "owner" || req.To != "panel" || len(req.Attachments) != 1 || req.Attachments[0].Name != "plan.md" {
		t.Fatalf("council got %+v", req)
	}
	parsed, err := council.ParseRequest(req.Body, council.DefaultConfig())
	if err != nil || parsed.Op != council.OpConvene || parsed.Question != "Which database?" ||
		strings.Join(parsed.Members, ",") != "claude-web,gemini-web,grok-web" || parsed.Chairman != "claude-web" {
		t.Fatalf("form %q parsed to %+v, %v", req.Body, parsed, err)
	}
	wantLine(t, out, "Convening panel as owner", req.ID)
	wantLine(t, out, "Approved", req.ID)
	last := -1
	for _, n := range notes {
		i := strings.Index(out, "Council: "+n)
		if i <= last {
			t.Fatalf("stage line %q missing or out of order:\n%s", n, out)
		}
		last = i
	}
	if i := strings.Index(out, "Recommendation: use Postgres."); i <= last {
		t.Fatalf("verdict missing or before the stages:\n%s", out)
	}
	for _, f := range []struct{ src, ext string }{{report, ".html"}, {card, ".png"}} {
		p := savedPath(t, out, f.ext)
		want, _ := os.ReadFile(f.src)
		if b, err := os.ReadFile(p); err != nil || !bytes.Equal(b, want) {
			t.Fatalf("saved %s = %q, %v", p, b, err)
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", p, st.Mode().Perm())
		}
		if st, _ := os.Stat(filepath.Dir(p)); st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v, want 0700", filepath.Dir(p), st.Mode().Perm())
		}
	}
}

// savedPath is the path on the "Saved " line of out ending in ext.
func savedPath(t *testing.T, out, ext string) string {
	t.Helper()
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(l, "Saved ") && strings.HasSuffix(l, ext) {
			return l[strings.LastIndex(l, " ")+1:]
		}
	}
	t.Fatalf("no Saved line ending %s in:\n%s", ext, out)
	return ""
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Without a terminal (a script or an agent's shell), or from a device that
// is not an admin device, tincan council leaves its request held and says
// it is waiting for the owner's approval.
func TestCouncilConveneWaitsForApproval(t *testing.T) {
	for _, tc := range []struct {
		name, owner string
		tty         bool
		want        string
	}{
		{"no terminal", "owner", false, "not run from a terminal"},
		{"not an admin device", "grokbot", true, "not an admin device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := ownerMesh(t, "council", tc.owner, tc.tty)
			out, errOut, err := runSplit(t, councilCmd(), "Which database?")
			if err != nil {
				t.Fatalf("council: %v\n%s%s", err, out, errOut)
			}
			held := heldIDs(t, m)
			if len(held) != 1 {
				t.Fatalf("held = %v, want the council request still held\n%s", held, out)
			}
			wantLine(t, out, "Convening council as "+tc.owner, held[0])
			wantLine(t, out, "Request "+held[0]+": held, waiting for the owner's approval", tc.want, "tincan approve "+held[0])
			if strings.Contains(out, "Approved") {
				t.Fatalf("approved without a terminal on an admin device:\n%s", out)
			}
		})
	}
}

// --json prints the reply's council-result block alone on stdout, and
// exits 0 for a completed council and 1 for a failed or declined one.
func TestCouncilJSON(t *testing.T) {
	for _, tc := range []struct {
		status envelope.Status
		code   int
	}{
		{envelope.StatusAnswered, 0},
		{envelope.StatusFailed, 1},
		{envelope.StatusDeclined, 1},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			_, c := ownerMesh(t, "council", "owner", true)
			block := `{"request_id":"x","status":"` + string(tc.status) + `","state":"s"}`
			took := fakeCouncil(t, c, []string{"Answers: 3 of 3 in"}, "Council says.\n\n```council-result\n"+block+"\n```\n", tc.status)
			out, errOut, err := runSplit(t, councilCmd(), "Which database?", "--json")
			<-took
			var got map[string]any
			if jerr := json.Unmarshal([]byte(out), &got); jerr != nil || got["status"] != string(tc.status) || got["request_id"] != "x" {
				t.Fatalf("stdout is not the block: %v\n%s", jerr, out)
			}
			if !strings.Contains(errOut, "Council: Answers: 3 of 3 in") {
				t.Fatalf("stage line not on stderr:\n%s", errOut)
			}
			ee, isExit := errors.AsType[*ExitError](err)
			switch {
			case tc.code == 0 && err != nil:
				t.Fatalf("exit %v, want 0", err)
			case tc.code != 0 && (!isExit || ee.Code != tc.code):
				t.Fatalf("exit %v, want code %d", err, tc.code)
			}
		})
	}
}

// council leaderboard sends the leaderboard form to the real Council
// service; --card saves the PNG it attaches and prints the path.
func TestCouncilLeaderboardCard(t *testing.T) {
	_, c := ownerMesh(t, "council", "owner", true)
	st, err := council.OpenStore(filepath.Join(t.TempDir(), "council.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := council.NewService(council.ServiceConfig{Relay: c, Config: council.DefaultConfig(), Store: st, ReportDir: t.TempDir(), Hold: time.Second, Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			svc.PollOnce(ctx)
		}
	}()
	defer func() { cancel(); <-done }()
	out, errOut, err := runSplit(t, councilCmd(), "leaderboard", "--category", "debugging", "--card")
	if err != nil {
		t.Fatalf("leaderboard: %v\n%s%s", err, out, errOut)
	}
	wantLine(t, out, "Council leaderboard (debugging)")
	p := savedPath(t, out, ".png")
	b, err := os.ReadFile(p)
	if err != nil || !bytes.HasPrefix(b, []byte("\x89PNG")) {
		t.Fatalf("card %s is not a PNG: %v", p, err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("card mode %v, want 0600", st.Mode().Perm())
	}
}
