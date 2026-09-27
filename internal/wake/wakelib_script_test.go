package wake

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests source examples/lib/tincan-wake-lib.sh from a small driver
// script, the way a per-CLI wake script does, and run it against a fake CLI
// and a fake tincan that only record what they were asked to do.

// fakeCLI answers --version, records each real run, and can sleep (with a
// child of its own, like a CLI running an MCP server) or fail.
const fakeCLI = `#!/bin/sh
if [ "${1:-}" = --version ]; then
  echo "${FAKE_VERSION:-Fake CLI 1.2.3}"
  exit 0
fi
echo run >> "$RUNS"
echo $$ > "$STATE_OUT/cli.pid"
if [ -n "${FAKE_SLEEP:-}" ]; then
  sleep "$FAKE_SLEEP" &
  echo $! > "$STATE_OUT/child.pid"
  wait
fi
exit "${FAKE_EXIT:-0}"
`

// fakeTincan records each call's argv on one line.
const fakeTincan = `#!/bin/sh
printf '%s\n' "$*" >> "$NOTICES"
`

// driver is a minimal per-CLI wake script on the library.
const driver = `set -eu
. "$TINCAN_WAKE_LIB"
tincan_wake_init fake
tincan_wake_begin
tincan_wake_check_binary "$FAKE_BIN" '^Fake CLI '
fake_mcp_list() {
  [ -z "${FAKE_LIST_FAIL:-}" ] || return 1
  cat "$MCP_LISTING"
}
tincan_wake_check_identity fake_mcp_list
tincan_wake_run "$FAKE_BIN" run "drain the inbox"
`

type libHarness struct {
	t                       *testing.T
	dir, state, config, bin string
	listing, runs, notices  string
	lib                     string
	env                     []string
}

func newLibHarness(t *testing.T) *libHarness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tincan-wake-lib.sh is a POSIX sh library")
	}
	lib, err := filepath.Abs(filepath.Join("..", "..", "examples", "lib", "tincan-wake-lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := &libHarness{t: t, dir: dir, lib: lib,
		state:   filepath.Join(dir, "state"),
		config:  filepath.Join(dir, "config", "fake.json"),
		bin:     filepath.Join(dir, "fake-cli"),
		listing: filepath.Join(dir, "listing"),
		runs:    filepath.Join(dir, "runs"),
		notices: filepath.Join(dir, "notices"),
	}
	mkdirs(t, filepath.Dir(h.config))
	if err := os.WriteFile(h.bin, []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tincan"), []byte(fakeTincan), 0o755); err != nil {
		t.Fatal(err)
	}
	h.setListing("agent-tincan\t" + h.config + "\ttincan mcp")
	return h
}

func (h *libHarness) setListing(lines ...string) {
	h.t.Helper()
	if err := os.WriteFile(h.listing, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

type libRun struct {
	err    error
	stderr string
	took   time.Duration
}

// run runs body (the driver by default) with the library sourced from
// TINCAN_WAKE_LIB, the harness environment, and env on top.
func (h *libHarness) run(body string, env ...string) libRun {
	h.t.Helper()
	script := filepath.Join(h.dir, "driver.sh")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + h.dir,
		"TINCAN_WAKE_LIB=" + h.lib,
		"TINCAN_WAKE_STATE_DIR=" + h.state,
		"TINCAN_BIN=" + filepath.Join(h.dir, "tincan"),
		"TINCAN_CONFIG=" + h.config,
		"FAKE_BIN=" + h.bin,
		"MCP_LISTING=" + h.listing,
		"RUNS=" + h.runs,
		"NOTICES=" + h.notices,
		"STATE_OUT=" + h.dir,
	}, h.env...)
	cmd.Env = append(cmd.Env, env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	return libRun{err: err, stderr: stderr.String(), took: time.Since(start)}
}

// count returns the number of lines in a recording file (0 if missing).
func (h *libHarness) count(file string) int {
	raw, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

func (h *libHarness) exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (h *libHarness) lockDir() string { return filepath.Join(h.state, "lock") }

func exitCode(err error) int {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestWakeLibRunsCLIAndReleasesLock(t *testing.T) {
	h := newLibHarness(t)
	r := h.run(driver)
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if n := h.count(h.runs); n != 1 {
		t.Fatalf("CLI ran %d times, want 1\nstderr:\n%s", n, r.stderr)
	}
	if h.exists(h.lockDir()) {
		t.Fatal("lock left behind after a clean run")
	}
	if h.count(h.notices) != 0 {
		t.Fatal("operator notice sent although no operator is configured")
	}
}

// deadPid returns the pid of a process that has exited and been reaped.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func writeLock(t *testing.T, dir string, pid int) {
	t.Helper()
	mkdirs(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWakeLibBreaksStaleLock(t *testing.T) {
	h := newLibHarness(t)
	writeLock(t, h.lockDir(), deadPid(t))
	r := h.run(driver)
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if n := h.count(h.runs); n != 1 {
		t.Fatalf("CLI ran %d times after a stale lock, want 1\nstderr:\n%s", n, r.stderr)
	}
	if !strings.Contains(r.stderr, "stale lock") {
		t.Fatalf("breaking the stale lock was not reported:\n%s", r.stderr)
	}
	if h.exists(h.lockDir()) {
		t.Fatal("lock left behind")
	}
}

func TestWakeLibLiveLockExitsWithoutRunning(t *testing.T) {
	h := newLibHarness(t)
	writeLock(t, h.lockDir(), os.Getpid()) // this test process is alive
	r := h.run(driver)
	if r.err != nil {
		t.Fatalf("a busy wake should exit 0: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if n := h.count(h.runs); n != 0 {
		t.Fatalf("CLI ran %d times while another run held the lock", n)
	}
	raw, err := os.ReadFile(filepath.Join(h.lockDir(), "pid"))
	if err != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the live run's lock was touched: %q, %v", raw, err)
	}
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func readPid(t *testing.T, p string) int {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestWakeLibTimeoutKillsCLIAndReleasesLock(t *testing.T) {
	h := newLibHarness(t)
	r := h.run(driver, "TINCAN_WAKE_TIMEOUT=1", "FAKE_SLEEP=60")
	if code := exitCode(r.err); code != 124 {
		t.Fatalf("exit = %d (%v), want 124 for a timeout\nstderr:\n%s", code, r.err, r.stderr)
	}
	if r.took > 20*time.Second {
		t.Fatalf("timed-out wake took %s", r.took)
	}
	if !strings.Contains(r.stderr, "timed out") {
		t.Fatalf("timeout not reported:\n%s", r.stderr)
	}
	if h.exists(h.lockDir()) {
		t.Fatal("lock left behind after a timeout")
	}
	cli, child := readPid(t, filepath.Join(h.dir, "cli.pid")), readPid(t, filepath.Join(h.dir, "child.pid"))
	deadline := time.Now().Add(5 * time.Second)
	for (alive(cli) || alive(child)) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive(cli) || alive(child) {
		t.Fatalf("CLI (%d alive=%v) or its child (%d alive=%v) survived the timeout", cli, alive(cli), child, alive(child))
	}
}

func TestWakeLibBacksOffAfterThreeFailures(t *testing.T) {
	h := newLibHarness(t)
	h.env = []string{"FAKE_EXIT=3", "TINCAN_WAKE_OPERATOR=ops"}
	for i := 1; i <= 3; i++ {
		r := h.run(driver)
		if code := exitCode(r.err); code != 3 {
			t.Fatalf("run %d: exit = %d, want the CLI's 3\nstderr:\n%s", i, code, r.stderr)
		}
		if i < 3 && h.count(h.notices) != 0 {
			t.Fatalf("operator notified after %d failures", i)
		}
	}
	if !h.exists(filepath.Join(h.state, "backoff")) {
		t.Fatal("no backoff marker after three failures")
	}
	for i := range 2 {
		r := h.run(driver)
		if r.err != nil {
			t.Fatalf("nudge %d during backoff: %v\nstderr:\n%s", i, r.err, r.stderr)
		}
		if !strings.Contains(r.stderr, "backing off") {
			t.Fatalf("nudge %d during backoff did not say so:\n%s", i, r.stderr)
		}
	}
	if n := h.count(h.runs); n != 3 {
		t.Fatalf("CLI ran %d times, want 3 (none during backoff)", n)
	}
	raw, _ := os.ReadFile(h.notices)
	if n := h.count(h.notices); n != 1 {
		t.Fatalf("operator notices = %d, want 1:\n%s", n, raw)
	}
	if !strings.HasPrefix(string(raw), "ask --notify ops ") {
		t.Fatalf("notice was not a tincan ask --notify to the operator: %q", raw)
	}

	// Once the backoff has run out, a failure backs off again without a
	// second notice, and a success clears it all.
	if err := os.WriteFile(filepath.Join(h.state, "backoff"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.run(driver); exitCode(r.err) != 3 || !h.exists(filepath.Join(h.state, "backoff")) {
		t.Fatalf("failure after the backoff ran out: %v, backoff set %v\nstderr:\n%s", r.err, h.exists(filepath.Join(h.state, "backoff")), r.stderr)
	}
	if n := h.count(h.notices); n != 1 {
		t.Fatalf("operator notices = %d after the second backoff, want still 1", n)
	}
	if err := os.WriteFile(filepath.Join(h.state, "backoff"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.run(driver, "FAKE_EXIT=0"); r.err != nil {
		t.Fatalf("success after backoff: %v\nstderr:\n%s", r.err, r.stderr)
	}
	for _, f := range []string{"backoff", "failures", "notified"} {
		if h.exists(filepath.Join(h.state, f)) {
			t.Errorf("%s not cleared by a successful run", f)
		}
	}
}

func TestWakeLibIdentityCheckStopsTheWake(t *testing.T) {
	cases := []struct {
		name    string
		listing func(h *libHarness) []string
		env     []string
		want    string
	}{
		{"second tincan server", func(h *libHarness) []string {
			return []string{"agent-tincan\t" + h.config + "\ttincan mcp", "tincan-old\t" + h.config + "\t/usr/local/bin/tincan mcp"}
		}, nil, "2 agent-tincan servers"},
		{"tincan by command only", func(h *libHarness) []string {
			return []string{"agent-tincan\t" + h.config + "\ttincan mcp", "helper\t\t/opt/bin/tincan mcp --channel"}
		}, nil, "2 agent-tincan servers"},
		{"another agent's config", func(h *libHarness) []string {
			return []string{"agent-tincan\t" + filepath.Join(h.dir, "config", "claude-code.json") + "\ttincan mcp"}
		}, nil, "claude-code.json"},
		{"no TINCAN_CONFIG on the entry", func(h *libHarness) []string {
			return []string{"agent-tincan\t\ttincan mcp"}
		}, nil, "TINCAN_CONFIG"},
		{"no tincan server", func(h *libHarness) []string {
			return []string{"github\t\tgh mcp"}
		}, nil, "0 agent-tincan servers"},
		{"extra server not allowed", func(h *libHarness) []string {
			return []string{"agent-tincan\t" + h.config + "\ttincan mcp", "github\t\tgh mcp", "fs\t\tnpx fs"}
		}, []string{"TINCAN_WAKE_ALLOWED_SERVERS=fs"}, "github"},
		{"listing fails", func(h *libHarness) []string { return nil }, []string{"FAKE_LIST_FAIL=1"}, "MCP servers"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newLibHarness(t)
			if l := c.listing(h); l != nil {
				h.setListing(l...)
			}
			env := append([]string{"TINCAN_WAKE_OPERATOR=ops"}, c.env...)
			r := h.run(driver, env...)
			if r.err == nil {
				t.Fatalf("wake ran with a bad MCP listing\nstderr:\n%s", r.stderr)
			}
			if n := h.count(h.runs); n != 0 {
				t.Fatalf("CLI ran %d times, want 0", n)
			}
			if !strings.Contains(r.stderr, c.want) {
				t.Fatalf("stderr lacks %q:\n%s", c.want, r.stderr)
			}
			if !h.exists(filepath.Join(h.state, "backoff")) {
				t.Fatal("refusal did not set backoff")
			}
			if n := h.count(h.notices); n != 1 {
				t.Fatalf("operator notices = %d, want 1", n)
			}
			if h.exists(h.lockDir()) {
				t.Fatal("lock left behind after a refusal")
			}
			// The next nudge backs off without listing or running anything.
			if r := h.run(driver, env...); r.err != nil || h.count(h.runs) != 0 || h.count(h.notices) != 1 {
				t.Fatalf("nudge after refusal: %v, runs %d, notices %d\nstderr:\n%s", r.err, h.count(h.runs), h.count(h.notices), r.stderr)
			}
		})
	}
}

// Servers the operator allows, and a config path reached through a symlink,
// pass the identity check.
func TestWakeLibIdentityCheckAllows(t *testing.T) {
	h := newLibHarness(t)
	link := filepath.Join(h.dir, "config-link")
	if err := os.Symlink(filepath.Dir(h.config), link); err != nil {
		t.Fatal(err)
	}
	h.setListing("github\t\tgh mcp", "agent-tincan\t"+filepath.Join(link, "fake.json")+"\ttincan mcp", "fs\t\tnpx fs")
	r := h.run(driver, "TINCAN_WAKE_ALLOWED_SERVERS=github, fs")
	if r.err != nil {
		t.Fatalf("wake: %v\nstderr:\n%s", r.err, r.stderr)
	}
	if n := h.count(h.runs); n != 1 {
		t.Fatalf("CLI ran %d times, want 1", n)
	}
}

func TestWakeLibBinaryCheck(t *testing.T) {
	h := newLibHarness(t)
	r := h.run(driver, "FAKE_VERSION=grok-cli 0.1 (community)", "TINCAN_WAKE_OPERATOR=ops")
	if r.err == nil || h.count(h.runs) != 0 {
		t.Fatalf("wrong binary ran: %v, runs %d\nstderr:\n%s", r.err, h.count(h.runs), r.stderr)
	}
	if !strings.Contains(r.stderr, "community") || !h.exists(filepath.Join(h.state, "backoff")) || h.count(h.notices) != 1 {
		t.Fatalf("wrong binary: backoff %v, notices %d\nstderr:\n%s", h.exists(filepath.Join(h.state, "backoff")), h.count(h.notices), r.stderr)
	}
}

// The confinement helper is the codex-wake.sh allowed-roots check: only
// canonical paths at or under an allowed root come out, one per line.
func TestWakeLibWriteRoots(t *testing.T) {
	h := newLibHarness(t)
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	proj := filepath.Join(allowed, "proj with space")
	outside := filepath.Join(base, "outside")
	sibling := filepath.Join(base, "allowed-evil")
	mkdirs(t, proj, outside, sibling)
	escape := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	roots := []string{proj, outside, escape, allowed + "/../outside", sibling, "relative/path", filepath.Join(allowed, "missing"), ""}

	body := `set -eu
. "$TINCAN_WAKE_LIB"
tincan_wake_init fake
roots=$(tincan_wake_write_roots "$ALLOWED" "$WRITE")
set --
while IFS= read -r d; do
  [ -n "$d" ] && set -- "$@" --add-dir "$d"
done <<EOF
$roots
EOF
for a in "$@"; do printf '%s\n' "$a"; done > "$STATE_OUT/argv"
`
	r := h.run(body, "ALLOWED="+allowed, "WRITE="+strings.Join(roots, ":"))
	if r.err != nil {
		t.Fatalf("driver: %v\nstderr:\n%s", r.err, r.stderr)
	}
	raw, err := os.ReadFile(filepath.Join(h.dir, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSuffix(string(raw), "\n")
	if want := "--add-dir\n" + canonical(t, proj); got != want {
		t.Fatalf("argv = %q, want %q\nstderr:\n%s", got, want, r.stderr)
	}
	for _, refused := range []string{outside, escape, allowed + "/../outside", sibling, "relative/path"} {
		if !strings.Contains(r.stderr, refused) {
			t.Errorf("refused root %q not reported on stderr:\n%s", refused, r.stderr)
		}
	}
}
