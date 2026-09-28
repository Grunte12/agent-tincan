package mcpserver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Hosts a tincan mcp server can run under, as far as reloading it goes.
const (
	HostClaudeCode = "claude-code"
	HostCodex      = "codex"
	HostCursor     = "cursor"
	HostGeneric    = "generic"
)

// HostKind names the host from the clientInfo name it sent at initialize
// (for example "claude-code", "codex-mcp-client" or "cursor-vscode").
// Launch records keep "name version"; only the name matters here.
func HostKind(client string) string {
	name, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(client)), " ")
	switch {
	case strings.Contains(name, "claude-code"):
		return HostClaudeCode
	case strings.Contains(name, "codex"):
		return HostCodex
	case strings.Contains(name, "cursor"):
		return HostCursor
	}
	return HostGeneric
}

// ReloadStep is how a host starts a fresh tincan mcp, phrased to follow
// "to use the new build," or "then".
func ReloadStep(kind string) string {
	switch kind {
	case HostClaudeCode:
		return "quit Claude Code and start it again (or reconnect the tincan server from /mcp)"
	case HostCodex:
		return "end this Codex session and start a new one, since Codex starts tincan mcp with each session"
	case HostCursor:
		return "turn the tincan server off and on in Cursor Settings > MCP, or restart Cursor"
	}
	return "reload the tincan MCP server in your app's settings, or quit and reopen the app"
}

// BuildWatch notices when the tincan binary this server was started from is
// replaced by another build, as tincan upgrade does, so the agent can be
// told to reload the server. Hosts own the server's process, so it never
// restarts itself.
type BuildWatch struct {
	// Every is the least time between looks at the file (default a minute).
	Every time.Duration
	// Now and ReadVersion stand in for the clock and for running
	// "<path> version" in tests.
	Now         func() time.Time
	ReadVersion func(ctx context.Context, path string) (string, error)

	path, running string

	mu       sync.Mutex
	lastLook time.Time
	stamp    os.FileInfo // the file as last looked at; nil if it was missing
	disk     string      // version of the file at stamp; "" until one is read
	reading  bool        // a version read is running outside mu
	told     map[string]bool

	// A file whose version read failed is retried after retryAt, backing
	// off with each failure.
	failed   os.FileInfo
	failures int
	retryAt  time.Time
}

// maxRetryWait caps the backoff between version reads of a file that
// failed to report one.
const maxRetryWait = time.Hour

func stampOf(path string) os.FileInfo {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return fi
}

// sameFile reports whether b is the file a was: the same file (tincan
// upgrade renames a new one into place), size and modification time.
func sameFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// NewBuildWatch watches path, the binary running as build running. It
// records the file as it is now, so it must be made at startup, before an
// upgrade can replace the file.
func NewBuildWatch(path, running string) *BuildWatch {
	return &BuildWatch{
		Every:       time.Minute,
		Now:         time.Now,
		ReadVersion: readVersion,
		path:        path,
		running:     running,
		stamp:       stampOf(path),
		told:        map[string]bool{},
	}
}

func readVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, path, "version")
	c.Env = append(os.Environ(), ProbeEnv+"=1")
	out, err := c.CombinedOutput()
	if err != nil {
		return "", err
	}
	lines := strings.Fields(string(out))
	if len(lines) == 0 {
		return "", fmt.Errorf("%s version printed nothing", path)
	}
	return lines[len(lines)-1], nil
}

// Notice returns the one-time line for a binary now on another build than
// this server runs, naming the reload step for host kind, or "". It looks at
// the file at most once per Every and runs it only when the file changed.
func (w *BuildWatch) Notice(ctx context.Context, kind string) string {
	if w == nil || w.path == "" {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.Now()
	if w.reading || (!w.lastLook.IsZero() && now.Sub(w.lastLook) < w.Every) {
		return ""
	}
	w.lastLook = now
	st := stampOf(w.path)
	if st == nil {
		return ""
	}
	if !sameFile(st, w.stamp) {
		if sameFile(st, w.failed) && now.Before(w.retryAt) {
			return ""
		}
		// Run the new file without holding mu, so other tool calls go on
		// meanwhile; they skip the look while this read runs. The read
		// outlives a cancelled tool call, so a cancel is not a failure.
		w.reading = true
		w.mu.Unlock()
		v, err := w.ReadVersion(context.WithoutCancel(ctx), w.path)
		w.mu.Lock()
		w.reading = false
		if err != nil {
			// Try this file again later, backing off so a build that
			// cannot report its version does not slow every look.
			if !sameFile(st, w.failed) {
				w.failed, w.failures = st, 0
			}
			w.failures++
			w.retryAt = now.Add(min(w.Every<<min(w.failures-1, 10), maxRetryWait))
			return ""
		}
		w.stamp, w.failed, w.failures = st, nil, 0
		w.disk = strings.TrimPrefix(strings.TrimSpace(v), "v")
	}
	running := strings.TrimPrefix(w.running, "v")
	if w.disk == "" || w.disk == running || w.told[w.disk] {
		return ""
	}
	w.told[w.disk] = true
	return fmt.Sprintf("Agent Tincan: these tools keep running tincan %s, but %s is now tincan %s (upgraded after this server started). To use the new build, %s.\n",
		running, w.path, w.disk, ReloadStep(kind))
}

// Watch adds the watch's notice to the next tool result after the binary
// changes. Only the server started from that binary (tincan mcp) gets it.
func Watch(w *BuildWatch) Option {
	return func(o *options) { o.watch = w }
}

// sessionHost is the host kind of the client on req's session.
func sessionHost(s mcp.Session) string {
	ss, ok := s.(*mcp.ServerSession)
	if !ok || ss == nil {
		return HostGeneric
	}
	p := ss.InitializeParams()
	if p == nil || p.ClientInfo == nil {
		return HostGeneric
	}
	return HostKind(p.ClientInfo.Name)
}

// watchMiddleware appends w's notice to tool results.
func watchMiddleware(w *BuildWatch) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != "tools/call" {
				return res, err
			}
			if r, ok := res.(*mcp.CallToolResult); ok && r != nil {
				if n := w.Notice(ctx, sessionHost(req.GetSession())); n != "" {
					r.Content = append(r.Content, &mcp.TextContent{Text: n})
				}
			}
			return res, err
		}
	}
}
