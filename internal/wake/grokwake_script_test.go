package wake

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// These tests run examples/grok-cli/grok-wake.sh with a fake grok that
// records what it was run with, the way codexwake_script_test.go does for
// codex, and a fake tincan that records operator notices. The shared
// library comes from examples/lib, where the script finds it in a repo
// checkout.

// fakeGrok answers --version, and "inspect --json" the way Grok Build
// does: the MCP servers in $GROK_HOME/config.toml, plus the Claude Code
// ones it imports from $HOME/.claude.json, plus plugins and hooks when
// FAKE_PLUGINS or FAKE_HOOKS is set. FAKE_TARGET overrides the command it
// reports for config.toml servers (by default the one each entry names), and FAKE_PROJECT_LAYER a project config layer.
// Every call appends the path it was run as to exe. A run records its argv
// (NUL separated), its HOME, GROK_HOME and auto-update setting, its working
// directory and the sandbox.toml it would load, then prints Grok's JSON
// result.
const fakeGrok = `#!/bin/sh
printf '%s\n' "$0" >> "$ARGV_DIR/exe"
if [ "${1:-}" = --version ]; then
  printf '%s\n' "$HOME" > "$ARGV_DIR/version.home"
  echo "${FAKE_VERSION:-grok 1.0.40 (eb1a2256660d) [stable]}"
  exit 0
fi
if [ "${1:-}" = inspect ]; then
  printf '%s\n' "$HOME" > "$ARGV_DIR/inspect.home"
  pwd -P > "$ARGV_DIR/inspect.cwd"
  servers=
  if [ -f "$GROK_HOME/config.toml" ]; then
    for n in $(sed -n 's/^\[mcp_servers\.\([^].]*\)\]$/\1/p' "$GROK_HOME/config.toml"); do
      target=$(awk -v n="$n" '/^\[/ { s = ($0 == "[mcp_servers." n "]") } s && /^command *=/ { sub(/^command *= *"/, ""); sub(/".*/, ""); print; exit }' "$GROK_HOME/config.toml")
      servers="$servers{\"name\":\"$n\",\"transport\":\"stdio\",\"target\":\"${FAKE_TARGET:-$target}\",\"source\":{\"type\":\"configToml\",\"path\":\"$GROK_HOME/config.toml\"}},"
    done
  fi
  if grep -q tincan "$HOME/.claude.json" 2>/dev/null; then
    servers="$servers{\"name\":\"claude-tincan\",\"transport\":\"stdio\",\"target\":\"tincan\",\"source\":{\"type\":\"claudeJson\",\"path\":\"$HOME/.claude.json\"}},"
  fi
  plugins=
  [ -z "${FAKE_PLUGINS:-}" ] || plugins='{"name":"helper"}'
  hooks=
  [ -z "${FAKE_HOOKS:-}" ] || hooks='{"event":"PreToolUse"}'
  layers="{\"role\":\"user\",\"path\":\"$GROK_HOME/config.toml\"}"
  [ -z "${FAKE_PROJECT_LAYER:-}" ] || layers="$layers,{\"role\":\"project\",\"path\":\"$FAKE_PROJECT_LAYER\"}"
  printf '{"grokVersion":"1.0.40","cwd":"%s","hooks":[%s],"plugins":[%s],"mcpServers":[%s],"configSources":{"layers":[%s]}}\n' "$(pwd -P)" "$hooks" "$plugins" "${servers%,}" "$layers"
  exit 0
fi
n=$(ls "$ARGV_DIR" 2>/dev/null | grep -c "^run\\." || true)
for a in "$@"; do printf '%s\0' "$a"; done > "$ARGV_DIR/run.$n"
printf '%s\n%s\n%s\n' "$HOME" "$GROK_HOME" "${GROK_DISABLE_AUTOUPDATER:-}" > "$ARGV_DIR/env.$n"
pwd -P > "$ARGV_DIR/cwd.$n"
cp "$GROK_HOME/sandbox.toml" "$ARGV_DIR/sandbox.$n" 2>/dev/null || true
sid=
prev=
for a in "$@"; do
  [ "$prev" = --session-id ] && sid=$a
  prev=$a
done
[ -z "${FAKE_SLEEP:-}" ] || sleep "$FAKE_SLEEP"
if [ -n "${FAKE_STDERR:-}" ]; then
  echo "$FAKE_STDERR" >&2
fi
if [ -n "${FAKE_STDOUT:-}" ]; then
  echo "$FAKE_STDOUT"
else
  printf '{"text":"done","stopReason":"end_turn","sessionId":"%s"}\n' "$sid"
fi
exit "${FAKE_EXIT:-0}"
`

type grokHarness struct {
	t                                      *testing.T
	dir, home, config, state, argv, notice string
	wakeHome, grokHome, workdir, sessions  string
	// bin holds the fake grok. The wake refuses a binary in the temp
	// directories a run can write, where t.TempDir is, so it is made under
	// the user's cache directory instead.
	bin string
}

// outsideTemp makes a directory under the user's cache directory, outside
// both the temp directories and the source tree, removed when the test
// ends. The test is skipped when there is no such directory, or when it
// resolves under a temp directory, since the wake would refuse a binary
// there.
func outsideTemp(t *testing.T) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache directory to hold the fake grok outside the temp directories: %v", err)
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Skipf("cannot create the user cache directory %s for the fake grok: %v", cache, err)
	}
	cache, err = filepath.EvalSymlinks(cache)
	if err != nil {
		t.Skipf("cannot resolve the user cache directory: %v", err)
	}
	for _, root := range tempRoots() {
		if cache == root || strings.HasPrefix(cache, root+string(filepath.Separator)) {
			t.Skipf("the user cache directory %s is under the temp directory %s, where the wake refuses a grok binary", cache, root)
		}
	}
	d, err := os.MkdirTemp(cache, "tincan-grokbin-")
	if err != nil {
		t.Skipf("cannot make a directory for the fake grok in %s: %v", cache, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// tempRoots lists, resolved, the temp directories the wake treats as
// writable by a run: TMPDIR, /tmp and /var/tmp and, on macOS, the per-user
// temp root above TMPDIR.
func tempRoots() []string {
	var roots []string
	add := func(p string) {
		if r, err := filepath.EvalSymlinks(p); err == nil && r != "/" {
			roots = append(roots, filepath.Clean(r))
		}
	}
	add(os.TempDir())
	add("/tmp")
	add("/var/tmp")
	if runtime.GOOS == "darwin" {
		add(filepath.Dir(filepath.Clean(os.TempDir())))
	}
	return roots
}

func newGrokHarness(t *testing.T) *grokHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("grok-wake.sh is a POSIX sh script")
	}
	dir := t.TempDir()
	h := &grokHarness{t: t, dir: dir,
		home:   filepath.Join(dir, "home"),
		config: filepath.Join(dir, "home", ".config", "tincan", "grok-cli.json"),
		state:  filepath.Join(dir, "state"),
		argv:   filepath.Join(dir, "argv"),
		notice: filepath.Join(dir, "notices"),
	}
	h.wakeHome = filepath.Join(h.home, ".config", "tincan", "grok-cli.wake")
	h.grokHome = filepath.Join(h.wakeHome, ".grok")
	h.workdir = filepath.Join(h.wakeHome, "work")
	h.sessions = filepath.Join(h.home, ".config", "tincan", "grok-cli.wake-sessions")
	h.bin = outsideTemp(t)
	mkdirs(t, h.argv, h.grokHome)
	if err := os.WriteFile(filepath.Join(h.bin, "grok"), []byte(fakeGrok), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tincan"), []byte(fakeTincan), 0o755); err != nil {
		t.Fatal(err)
	}
	// The owner's own Claude Code config names agent-tincan with the
	// owner's identity: Grok Build would import it if the wake ran with
	// the owner's HOME.
	h.write(filepath.Join(h.home, ".claude.json"), `{"mcpServers":{"agent-tincan":{"command":"tincan","args":["mcp"],"env":{"TINCAN_CONFIG":"/Users/owner/.config/tincan/config.json"}}}}`)
	h.write(h.config, `{"agent":"grok-cli","relay":"http://relay"}`)
	h.writeConfigTOML(h.tincanTable(h.config))
	h.write(filepath.Join(h.grokHome, "auth.json"), `{"token":"dummy"}`)
	return h
}

func (h *grokHarness) write(path, body string) {
	h.t.Helper()
	mkdirs(h.t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// tincanTable is the config.toml entry "grok mcp add -e" writes.
func (h *grokHarness) tincanTable(config string) string {
	return "[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nargs = [\"mcp\"]\n\n[mcp_servers.agent-tincan.env]\nTINCAN_CONFIG = \"" + config + "\"\n"
}

func (h *grokHarness) writeConfigTOML(body string) {
	h.write(filepath.Join(h.grokHome, "config.toml"), body)
}

type grokRun struct {
	err            error
	stdout, stderr string
}

func (h *grokHarness) run(env ...string) grokRun {
	h.t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "grok-cli", "grok-wake.sh"))
	if err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.home,
		"GROK_BIN=" + filepath.Join(h.bin, "grok"),
		"TINCAN_BIN=" + filepath.Join(h.dir, "tincan"),
		"TINCAN_WAKE_STATE_DIR=" + h.state,
		"TINCAN_WAKE_OPERATOR=ops",
		"TINCAN_CONFIG=" + h.config,
		"ARGV_DIR=" + h.argv,
		"NOTICES=" + h.notice,
		"TINCAN_WAITING=2",
	}, slices.DeleteFunc(env, func(e string) bool { return e == "" })...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	return grokRun{err: err, stdout: stdout.String(), stderr: stderr.String()}
}

// runs returns the argv of every grok run so far, in order.
func (h *grokHarness) runs() []wakeRun {
	h.t.Helper()
	var out []wakeRun
	for i := 0; ; i++ {
		raw, err := os.ReadFile(filepath.Join(h.argv, "run."+strconv.Itoa(i)))
		if err != nil {
			return out
		}
		out = append(out, wakeRun{argv: strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")})
	}
}

func (h *grokHarness) file(name string) string {
	raw, _ := os.ReadFile(filepath.Join(h.argv, name))
	return string(raw)
}

func (h *grokHarness) notices() int {
	raw, err := os.ReadFile(h.notice)
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

func (h *grokHarness) backoff() bool {
	_, err := os.Stat(filepath.Join(h.state, "backoff"))
	return err == nil
}

// refused checks that the wake ran nothing, failed, backed off and told
// the operator once, saying want.
func (h *grokHarness) refused(r grokRun, want string) {
	h.t.Helper()
	if r.err == nil {
		h.t.Fatalf("wake succeeded, want a refusal\nstderr:\n%s", r.stderr)
	}
	if n := len(h.runs()); n != 0 {
		h.t.Fatalf("grok ran %d times, want 0", n)
	}
	if !strings.Contains(r.stderr, want) {
		h.t.Fatalf("stderr lacks %q:\n%s", want, r.stderr)
	}
	if !h.backoff() || h.notices() != 1 {
		h.t.Fatalf("backoff %v, notices %d; want a backoff and one notice", h.backoff(), h.notices())
	}
}

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// The wake runs Grok Build headless with approvals on, in its sandbox
// profile, in the workdir, as a new session whose id it chose and
// recorded, with HOME and GROK_HOME in the wake home, so the owner's
// Claude Code config (which names agent-tincan) is never imported.
func TestGrokWakeRunsSandboxedInItsWakeHome(t *testing.T) {
	h := newGrokHarness(t)
	r := h.run()
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("grok ran %d times, want 1\nstderr:\n%s", len(runs), r.stderr)
	}
	a := runs[0]
	for flag, want := range map[string]string{
		"--output-format": "json",
		"--sandbox":       "tincan-wake",
		"--cwd":           canonical(t, h.workdir),
	} {
		if got := a.flagValues(flag); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want %q: %q", flag, got, want, a.argv)
		}
	}
	if !slices.Contains(a.argv, "--always-approve") {
		t.Errorf("argv lacks --always-approve: %q", a.argv)
	}
	prompts := a.flagValues("-p")
	if len(prompts) != 1 {
		t.Fatalf("argv lacks one -p prompt: %q", a.argv)
	}
	for _, want := range []string{"2 Agent Tincan item(s) waiting", "check_inbox", "call reply", "until it returns nothing waiting"} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("drain prompt lacks %q:\n%s", want, prompts[0])
		}
	}
	sid := a.flagValues("--session-id")
	if len(sid) != 1 || !uuidShape.MatchString(sid[0]) {
		t.Fatalf("--session-id = %q, want one lowercase UUID", sid)
	}
	env := strings.Split(h.file("env.0"), "\n")
	if env[0] != h.wakeHome || env[1] != h.grokHome || env[2] != "1" {
		t.Fatalf("grok ran with HOME %q GROK_HOME %q GROK_DISABLE_AUTOUPDATER %q, want the wake home", env[0], env[1], env[2])
	}
	if got := strings.TrimSpace(h.file("inspect.home")); got != h.wakeHome {
		t.Fatalf("the identity check listed servers with HOME %q, want the wake home", got)
	}
	if got := strings.TrimSpace(h.file("inspect.cwd")); got != canonical(t, h.workdir) {
		t.Fatalf("the identity check listed servers from %q, want the workdir", got)
	}
	if got := strings.TrimSpace(h.file("cwd.0")); got != canonical(t, h.workdir) {
		t.Fatalf("grok ran in %q, want the workdir", got)
	}
	if got := strings.TrimSpace(h.file("version.home")); got != h.wakeHome {
		t.Fatalf("the version check ran with HOME %q, want the wake home", got)
	}
	// The sandbox profile extends workspace and opens the attachments
	// directory tincan mcp saves received files in.
	sb := h.file("sandbox.0")
	att := filepath.Join(canonical(t, filepath.Dir(h.config)), "attachments", "grok-cli")
	for _, want := range []string{"[profiles.tincan-wake]", `extends = "workspace"`, `"` + att + `"`} {
		if !strings.Contains(sb, want) {
			t.Errorf("sandbox.toml lacks %q:\n%s", want, sb)
		}
	}
	// The session id is recorded, with the workdir, in a private list
	// beside the teammate's config, for the history agent.
	st, err := os.Stat(h.sessions)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("session list mode %v, want 0600", st.Mode().Perm())
	}
	list, _ := os.ReadFile(h.sessions)
	if !strings.Contains(string(list), "\n"+sid[0]+"\n") || !strings.Contains(string(list), "workdir "+canonical(t, h.workdir)+"\n") {
		t.Fatalf("session list lacks the id or the workdir:\n%s", list)
	}
	// During a run ~ is the wake home, so the tincan commands in the
	// standing instructions (TINCAN_CONFIG=~/.config/tincan/grok-cli.json)
	// must still reach the teammate's config: the wake home links it.
	link := filepath.Join(h.wakeHome, ".config", "tincan")
	if target, err := os.Readlink(link); err != nil || target != filepath.Dir(h.config) {
		t.Fatalf("wake home link %s -> %q (%v), want the config directory", link, target, err)
	}
	if !strings.Contains(r.stdout, `"sessionId"`) {
		t.Fatalf("grok's output not passed to the listener log:\n%s", r.stdout)
	}
	// A second wake starts a new session with a new id.
	if r := h.run(); r.err != nil {
		t.Fatalf("second wake: %v\n%s", r.err, r.stderr)
	}
	if runs := h.runs(); len(runs) != 2 || runs[1].flagValues("--session-id")[0] == sid[0] {
		t.Fatalf("second wake reused the session id: %q", runs)
	}
}

// A grok on PATH that is not Grok Build (the community grok-cli answers
// --version with a bare number) is refused once, with a backoff.
func TestGrokWakeRefusesCommunityGrok(t *testing.T) {
	h := newGrokHarness(t)
	h.refused(h.run("FAKE_VERSION=1.0.1"), "not the expected tool")
	// Backing off: no second run and no second notice.
	if r := h.run("FAKE_VERSION=1.0.1"); r.err != nil || len(h.runs()) != 0 || h.notices() != 1 {
		t.Fatalf("while backing off: err %v, runs %d, notices %d", r.err, len(h.runs()), h.notices())
	}
}

func TestGrokWakeIdentityCheck(t *testing.T) {
	other := "/Users/owner/.config/tincan/config.json"
	for _, tc := range []struct {
		name  string
		setup func(h *grokHarness)
		env   string
		want  string
	}{
		{"another teammate's config", func(h *grokHarness) { h.writeConfigTOML(h.tincanTable(other)) }, "", "not this wake's"},
		{"no TINCAN_CONFIG", func(h *grokHarness) {
			h.writeConfigTOML("[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nargs = [\"mcp\"]\n")
		}, "", "sets no TINCAN_CONFIG"},
		{"no tincan server", func(h *grokHarness) { h.writeConfigTOML("") }, "", "0 agent-tincan servers"},
		{"a Claude Code import in the wake home", func(h *grokHarness) {
			h.write(filepath.Join(h.wakeHome, ".claude.json"), `{"mcpServers":{"agent-tincan":{"command":"tincan"}}}`)
		}, "", "2 agent-tincan servers"},
		{"an unvetted server", func(h *grokHarness) {
			h.writeConfigTOML(h.tincanTable(h.config) + "\n[mcp_servers.files]\ncommand = \"npx\"\n")
		}, "", "did not allow: files"},
		{"a listed command that is not the config entry's", func(*grokHarness) {}, "FAKE_TARGET=sh", "sets no TINCAN_CONFIG"},
		{"a project config.toml in the workdir", func(h *grokHarness) {
			h.write(filepath.Join(h.workdir, ".grok", "config.toml"), "[mcp_servers.agent-tincan]\ncommand = \"sh\"\n")
		}, "", "project MCP config"},
		{"a project .mcp.json above the workdir", func(h *grokHarness) {
			h.write(filepath.Join(h.wakeHome, ".mcp.json"), `{"mcpServers":{}}`)
		}, "", "project MCP config"},
		{"a project layer Grok reports", func(*grokHarness) {}, "FAKE_PROJECT_LAYER=/elsewhere/.grok/config.toml", "project config"},
		{"a plugin", func(*grokHarness) {}, "FAKE_PLUGINS=1", "plugins or hooks"},
		{"a hook", func(*grokHarness) {}, "FAKE_HOOKS=1", "plugins or hooks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGrokHarness(t)
			tc.setup(h)
			h.refused(h.run(tc.env), tc.want)
		})
	}
}

// The inline env table form is read too, and an allowed extra server runs.
func TestGrokWakeInlineEnvAndAllowedServer(t *testing.T) {
	h := newGrokHarness(t)
	h.writeConfigTOML("[mcp_servers.agent-tincan]\ncommand = \"tincan\"\nargs = [\"mcp\"]\nenv = { TINCAN_CONFIG = \"" + h.config + "\" }\n\n[mcp_servers.files]\ncommand = \"npx\"\n")
	if r := h.run("TINCAN_WAKE_ALLOWED_SERVERS=files"); r.err != nil || len(h.runs()) != 1 {
		t.Fatalf("wake: %v, runs %d\n%s", r.err, len(h.runs()), r.stderr)
	}
}

// Without jq, python3 reads grok inspect --json and the teammate's config
// the same way.
func TestGrokWakeWithPython3(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	h := newGrokHarness(t)
	if r := h.run("TINCAN_GROK_JSON_TOOL=python3"); r.err != nil || len(h.runs()) != 1 {
		t.Fatalf("wake: %v, runs %d\n%s", r.err, len(h.runs()), r.stderr)
	}
	if !strings.Contains(h.file("sandbox.0"), filepath.Join("attachments", "grok-cli")) {
		t.Fatalf("attachments dir not read from the config with python3:\n%s", h.file("sandbox.0"))
	}
	h2 := newGrokHarness(t)
	h2.write(filepath.Join(h2.wakeHome, ".claude.json"), `{"mcpServers":{"agent-tincan":{"command":"tincan"}}}`)
	h2.refused(h2.run("TINCAN_GROK_JSON_TOOL=python3"), "2 agent-tincan servers")
	h3 := newGrokHarness(t)
	h3.refused(h3.run("TINCAN_GROK_JSON_TOOL=python3", "FAKE_HOOKS=1"), "plugins or hooks")
	h4 := newGrokHarness(t)
	h4.refused(h4.run("TINCAN_GROK_JSON_TOOL=python3", "FAKE_PROJECT_LAYER=/elsewhere/.grok/config.toml"), "project config")
}

// Operator write roots under an allowed root become read_write entries of
// the sandbox profile; anything else is left out.
func TestGrokWakeWriteRoots(t *testing.T) {
	h := newGrokHarness(t)
	code := filepath.Join(h.home, "code")
	inside, outside := filepath.Join(code, "proj"), filepath.Join(h.dir, "elsewhere")
	mkdirs(t, inside, outside)
	r := h.run("TINCAN_GROK_WRITE_ROOTS=" + inside + ":" + outside)
	if r.err != nil {
		t.Fatalf("wake: %v\n%s", r.err, r.stderr)
	}
	sb := h.file("sandbox.0")
	if !strings.Contains(sb, `"`+canonical(t, inside)+`"`) || strings.Contains(sb, canonical(t, outside)) {
		t.Fatalf("sandbox.toml read_write wrong:\n%s", sb)
	}
	if !strings.Contains(r.stderr, "refusing write root "+outside) {
		t.Fatalf("outside root not reported:\n%s", r.stderr)
	}
}

// Without a login in the wake home or XAI_API_KEY the wake refuses before
// running; with the key it runs.
func TestGrokWakeNeedsALogin(t *testing.T) {
	h := newGrokHarness(t)
	if err := os.Remove(filepath.Join(h.grokHome, "auth.json")); err != nil {
		t.Fatal(err)
	}
	h.refused(h.run(), "grok login")
	h2 := newGrokHarness(t)
	if err := os.Remove(filepath.Join(h2.grokHome, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if r := h2.run("XAI_API_KEY=dummy"); r.err != nil || len(h2.runs()) != 1 {
		t.Fatalf("with XAI_API_KEY: %v, runs %d\n%s", r.err, len(h2.runs()), r.stderr)
	}
}

// A run Grok fails for authentication backs off at once and tells the
// operator once; a later wake inside the backoff runs nothing.
func TestGrokWakeAuthFailureBacksOff(t *testing.T) {
	h := newGrokHarness(t)
	r := h.run("FAKE_EXIT=1", `FAKE_STDOUT={"type":"error","message":"Couldn't start session: not authenticated, run grok login"}`)
	if r.err == nil || len(h.runs()) != 1 {
		t.Fatalf("wake: %v, runs %d", r.err, len(h.runs()))
	}
	if !h.backoff() || h.notices() != 1 {
		t.Fatalf("backoff %v, notices %d after an auth failure", h.backoff(), h.notices())
	}
	h.run()
	if len(h.runs()) != 1 || h.notices() != 1 {
		t.Fatalf("ran again or notified again while backing off: runs %d notices %d", len(h.runs()), h.notices())
	}
	// "401" counts only as a whole number in Grok's error message or its
	// stderr, never inside a token count or a timestamp.
	tools := []string{"jq", "python3"}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			continue
		}
		h3 := newGrokHarness(t)
		r = h3.run("TINCAN_GROK_JSON_TOOL="+tool, "FAKE_EXIT=1", `FAKE_STDOUT={"type":"error","message":"stream closed","usage":{"total_tokens":54012}}`, "FAKE_STDERR=2026-09-27T10:00:00.401Z request failed")
		if r.err == nil || h3.backoff() || h3.notices() != 0 {
			t.Fatalf("%s: a non-auth failure backed off at once: err %v, backoff %v, notices %d", tool, r.err, h3.backoff(), h3.notices())
		}
		for _, msg := range []string{"401 Unauthorized", "request failed: HTTP 401"} {
			h4 := newGrokHarness(t)
			if r := h4.run("TINCAN_GROK_JSON_TOOL="+tool, "FAKE_EXIT=1", `FAKE_STDOUT={"type":"error","message":"`+msg+`"}`); r.err == nil || !h4.backoff() || h4.notices() != 1 {
				t.Fatalf("%s, %q: backoff %v, notices %d, want an auth backoff", tool, msg, h4.backoff(), h4.notices())
			}
		}
	}
	// Words in a successful answer never count as an auth failure.
	h2 := newGrokHarness(t)
	if r := h2.run(`FAKE_STDOUT={"text":"not authenticated","sessionId":"x"}`); r.err != nil || h2.backoff() {
		t.Fatalf("a successful run backed off: %v", r.err)
	}
}

// A run the timeout kills keeps its recorded id, so the history agent
// still leaves it out.
func TestGrokWakeRecordsTheIdBeforeRunning(t *testing.T) {
	h := newGrokHarness(t)
	r := h.run("TINCAN_WAKE_TIMEOUT=1", "FAKE_SLEEP=10")
	if r.err == nil {
		t.Fatalf("a timed-out run succeeded\n%s", r.stderr)
	}
	runs := h.runs()
	if len(runs) != 1 {
		t.Fatalf("runs %d", len(runs))
	}
	list, _ := os.ReadFile(h.sessions)
	if !strings.Contains(string(list), runs[0].flagValues("--session-id")[0]) {
		t.Fatalf("timed-out run's id not recorded:\n%s", list)
	}
}

// The wake refuses a wake home that is the owner's own home, where Grok
// would import the owner's other MCP servers.
func TestGrokWakeRefusesTheOwnersHome(t *testing.T) {
	h := newGrokHarness(t)
	h.refused(h.run("TINCAN_GROK_WAKE_HOME="+h.home), "the owner's home")
}

// With the npm install, grok on PATH is a node bootstrap that runs
// $GROK_HOME/bin/grok, which under the wake's GROK_HOME is a file a run
// can write. The wake pins the native binary the owner's Grok home points
// at and runs that file for the version check, the listing and the run,
// and removes a grok the wake home holds.
func TestGrokWakePinsTheNativeBinary(t *testing.T) {
	h := newGrokHarness(t)
	npm := filepath.Join(h.dir, "npm", "bin")
	h.write(filepath.Join(npm, "grok"), "#!/usr/bin/env node\nrequire('../lib/grok-bootstrap.js')\n")
	if err := os.Chmod(filepath.Join(npm, "grok"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The owner's Grok home is outside temp, where a real one would be.
	ownerGrok := filepath.Join(h.bin, ".grok")
	ownerBin := filepath.Join(ownerGrok, "bin")
	h.write(filepath.Join(ownerBin, "grok-1.0.40"), fakeGrok)
	if err := os.Chmod(filepath.Join(ownerBin, "grok-1.0.40"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("grok-1.0.40", filepath.Join(ownerBin, "grok")); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(h.grokHome, "bin", "grok")
	h.write(planted, "#!/bin/sh\necho planted >> \"$ARGV_DIR/planted\"\n")
	r := h.run("GROK_BIN=", "GROK_HOME="+ownerGrok, "PATH="+npm+":"+os.Getenv("PATH"))
	if r.err != nil || len(h.runs()) != 1 {
		t.Fatalf("wake: %v, runs %d\n%s", r.err, len(h.runs()), r.stderr)
	}
	native := filepath.Join(canonical(t, ownerBin), "grok-1.0.40")
	exes := strings.Fields(h.file("exe"))
	if len(exes) != 3 {
		t.Fatalf("grok called %d times, want version, inspect and run: %q", len(exes), exes)
	}
	for _, e := range exes {
		if e != native {
			t.Fatalf("grok ran as %q, want the pinned native binary %q", e, native)
		}
	}
	if _, err := os.Stat(planted); !os.IsNotExist(err) {
		t.Fatalf("the wake home's bin/grok is still there: %v", err)
	}
	// A bootstrap with no native binary in the owner's Grok home is
	// refused, never run.
	h2 := newGrokHarness(t)
	h2.write(filepath.Join(h2.dir, "npm", "grok"), "#!/usr/bin/env node\n")
	if err := os.Chmod(filepath.Join(h2.dir, "npm", "grok"), 0o755); err != nil {
		t.Fatal(err)
	}
	h2.refused(h2.run("GROK_BIN="+filepath.Join(h2.dir, "npm", "grok")), "native")
}

// A grok binary the sandboxed run could write is refused.
func TestGrokWakeRefusesAWritableBinary(t *testing.T) {
	for _, where := range []func(h *grokHarness) string{
		func(h *grokHarness) string { return filepath.Join(h.grokHome, "bin", "grok") },
		func(h *grokHarness) string { return filepath.Join(h.workdir, "grok") },
	} {
		h := newGrokHarness(t)
		bin := where(h)
		h.write(bin, fakeGrok)
		if err := os.Chmod(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		h.refused(h.run("GROK_BIN="+bin), "the sandboxed run can write")
	}
}

// By default the wake keeps its state beside the teammate's config, outside
// the temp directories the sandboxed run can write.
func TestGrokWakeStateOutsideTemp(t *testing.T) {
	h := newGrokHarness(t)
	if r := h.run("TINCAN_WAKE_STATE_DIR="); r.err != nil || len(h.runs()) != 1 {
		t.Fatalf("wake: %v, runs %d\n%s", r.err, len(h.runs()), r.stderr)
	}
	st, err := os.Stat(filepath.Join(filepath.Dir(h.config), "grok-cli.wake-state"))
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("state dir beside the config: %v %v", st, err)
	}
}

// The run can write the temp directories too, so a grok binary under
// TMPDIR (or /tmp) is refused.
func TestGrokWakeRefusesABinaryInTemp(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "grok")
	if err := os.WriteFile(bin, []byte(fakeGrok), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newGrokHarness(t)
	h.refused(h.run("GROK_BIN="+bin, "TMPDIR="+tmp), "the sandboxed run can write")

	if _, err := os.Stat("/tmp"); err == nil {
		d, err := os.MkdirTemp("/tmp", "tincan-grok-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		bin := filepath.Join(d, "grok")
		if err := os.WriteFile(bin, []byte(fakeGrok), 0o755); err != nil {
			t.Fatal(err)
		}
		h2 := newGrokHarness(t)
		h2.refused(h2.run("GROK_BIN="+bin, "TMPDIR="), "the sandboxed run can write")
	}
}
