package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testClientSecret = "test-client-secret-9f3a"
	testRefreshToken = "test-refresh-token-71bd"
	testAccessToken  = "test-access-token-c40e"
)

// writeCreds writes a credentials file with the given mode.
func writeCreds(t *testing.T, mode os.FileMode, fields map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cws-oauth.json")
	b, _ := json.Marshal(fields)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func fullCreds() map[string]string {
	return map[string]string{
		"client_id":     "test-client.apps.googleusercontent.com",
		"client_secret": testClientSecret,
		"refresh_token": testRefreshToken,
		"item_id":       "testitem",
	}
}

// storeZip writes a zip holding a manifest with version.
func storeZip(t *testing.T, version string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("manifest.json")
	json.NewEncoder(w).Encode(map[string]any{"manifest_version": 3, "name": "t", "version": version})
	zw.Close()
	p := filepath.Join(t.TempDir(), "store.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type cwsCall struct {
	Method, Path, Query, Auth, APIVersion, ContentType string
	BodyLen                                            int
}

// fakeCWS serves the token endpoint and the Web Store API, recording each
// store call. draftVersion is the store's crxVersion ("" leaves it out).
type fakeCWS struct {
	mu            sync.Mutex
	calls         []cwsCall
	draftVersion  string
	tokenStatus   int
	publishStatus []string
}

func (f *fakeCWS) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/token" {
			form, _ := url.ParseQuery(string(body))
			if f.tokenStatus != 0 {
				w.WriteHeader(f.tokenStatus)
				// A hostile or careless error body echoing the secret.
				io.WriteString(w, `{"error":"invalid_grant","echo":"`+form.Get("refresh_token")+`"}`)
				return
			}
			switch form.Get("grant_type") {
			case "refresh_token":
				if form.Get("refresh_token") != testRefreshToken || form.Get("client_secret") != testClientSecret {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				json.NewEncoder(w).Encode(map[string]string{"access_token": testAccessToken})
			case "authorization_code":
				if form.Get("code") != "test-code" || form.Get("redirect_uri") == "" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				json.NewEncoder(w).Encode(map[string]string{"access_token": testAccessToken, "refresh_token": "new-refresh-token-55aa"})
			}
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, cwsCall{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("x-goog-api-version"), r.Header.Get("Content-Type"), len(body)})
		f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			m := map[string]any{"id": "testitem", "uploadState": "SUCCESS"}
			if f.draftVersion != "" {
				m["crxVersion"] = f.draftVersion
			}
			json.NewEncoder(w).Encode(m)
		case http.MethodPut:
			json.NewEncoder(w).Encode(map[string]any{"id": "testitem", "uploadState": "SUCCESS"})
		case http.MethodPost:
			status := f.publishStatus
			if status == nil {
				status = []string{"OK"}
			}
			json.NewEncoder(w).Encode(map[string]any{"status": status})
		}
	}))
	t.Cleanup(srv.Close)
	oldAPI, oldToken, oldAuth := cwsAPIBase, cwsTokenURL, cwsAuthURL
	cwsAPIBase, cwsTokenURL, cwsAuthURL = srv.URL, srv.URL+"/token", srv.URL+"/auth"
	t.Cleanup(func() { cwsAPIBase, cwsTokenURL, cwsAuthURL = oldAPI, oldToken, oldAuth })
}

func assertNoSecrets(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{testClientSecret, testRefreshToken, testAccessToken, "new-refresh-token-55aa"} {
		if strings.Contains(s, secret) {
			t.Fatalf("output shows a secret %q:\n%s", secret, s)
		}
	}
}

// runReleaseTools runs tincan release-tools with args through the command
// tree and returns stdout plus the error text.
func runReleaseTools(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"release-tools"}, args...))
	err := root.ExecuteContext(context.Background())
	s := out.String()
	if err != nil {
		s += "\nerror: " + err.Error()
	}
	return s, err
}

// cws-upload uploads, then publishes, with the bearer token and API version
// header on every call, and prints no secret.
func TestCWSUploadAndPublish(t *testing.T) {
	f := &fakeCWS{draftVersion: "0.5.0"}
	f.start(t)
	creds := writeCreds(t, 0o600, fullCreds())
	zipPath := storeZip(t, "0.6.0")
	out, err := runReleaseTools(t, "cws-upload", "--credentials", creds, "--publish", zipPath)
	if err != nil {
		t.Fatal(out)
	}
	assertNoSecrets(t, out)
	if !strings.Contains(out, "uploaded version 0.6.0") || !strings.Contains(out, "published: OK") {
		t.Fatalf("output:\n%s", out)
	}
	want := []struct{ method, path string }{
		{"GET", "/chromewebstore/v1.1/items/testitem"},
		{"PUT", "/upload/chromewebstore/v1.1/items/testitem"},
		{"POST", "/chromewebstore/v1.1/items/testitem/publish"},
	}
	if len(f.calls) != len(want) {
		t.Fatalf("calls = %+v", f.calls)
	}
	for i, w := range want {
		c := f.calls[i]
		if c.Method != w.method || c.Path != w.path {
			t.Fatalf("call %d = %s %s; want %s %s", i, c.Method, c.Path, w.method, w.path)
		}
		if c.Auth != "Bearer "+testAccessToken || c.APIVersion != "2" {
			t.Fatalf("call %d headers: auth %q, x-goog-api-version %q", i, c.Auth, c.APIVersion)
		}
	}
	if f.calls[0].Query != "projection=DRAFT" {
		t.Fatalf("status query = %q", f.calls[0].Query)
	}
	if put := f.calls[1]; put.ContentType != "application/zip" || put.BodyLen == 0 {
		t.Fatalf("upload: content type %q, %d bytes", put.ContentType, put.BodyLen)
	}
}

// When the store already has the zip's version or a newer one, the upload
// and publish are skipped and the command succeeds, so make release goes on.
func TestCWSUploadSkipsVersionNotNewer(t *testing.T) {
	for _, store := range []string{"0.5.0", "0.5.1"} {
		t.Run(store, func(t *testing.T) {
			f := &fakeCWS{draftVersion: store}
			f.start(t)
			out, err := runReleaseTools(t, "cws-upload", "--credentials", writeCreds(t, 0o600, fullCreds()), "--publish", storeZip(t, "0.5.0"))
			if err != nil {
				t.Fatal(out)
			}
			if !strings.Contains(out, "nothing is uploaded") {
				t.Fatalf("output:\n%s", out)
			}
			if len(f.calls) != 1 || f.calls[0].Method != "GET" {
				t.Fatalf("calls = %+v; want only the status GET", f.calls)
			}
		})
	}
}

// --dry-run checks the token and the store version and changes nothing.
func TestCWSUploadDryRun(t *testing.T) {
	f := &fakeCWS{draftVersion: "0.5.0"}
	f.start(t)
	out, err := runReleaseTools(t, "cws-upload", "--credentials", writeCreds(t, 0o600, fullCreds()), "--dry-run", "--publish", storeZip(t, "0.6.0"))
	if err != nil {
		t.Fatal(out)
	}
	if !strings.Contains(out, "dry run: would upload") {
		t.Fatalf("output:\n%s", out)
	}
	if len(f.calls) != 1 || f.calls[0].Method != "GET" {
		t.Fatalf("calls = %+v; want only the status GET", f.calls)
	}
}

// A credentials file anyone else can read or write is refused before any
// network call.
func TestCWSCredentialsPermissions(t *testing.T) {
	f := &fakeCWS{}
	f.start(t)
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
		creds := writeCreds(t, mode, fullCreds())
		for _, args := range [][]string{
			{"cws-upload", "--credentials", creds, storeZip(t, "0.6.0")},
			{"cws-auth", "--credentials", creds, "--port", "0", "--timeout", "1s"},
		} {
			out, err := runReleaseTools(t, args...)
			if err == nil || !strings.Contains(out, "chmod 600") {
				t.Fatalf("%s with mode %04o: %v\n%s", args[0], mode, err, out)
			}
			assertNoSecrets(t, out)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %+v", f.calls)
	}
}

// The credentials path comes from TINCAN_CWS_CREDENTIALS when set.
func TestCWSCredentialsEnv(t *testing.T) {
	t.Setenv(cwsCredentialsEnv, "/x/creds.json")
	if got := defaultCWSCredentials(); got != "/x/creds.json" {
		t.Fatalf("defaultCWSCredentials() = %q", got)
	}
}

// Error bodies that echo a secret are redacted, and an expired refresh
// token points at cws-auth.
func TestCWSUploadErrorsHideSecrets(t *testing.T) {
	f := &fakeCWS{tokenStatus: http.StatusBadRequest}
	f.start(t)
	out, err := runReleaseTools(t, "cws-upload", "--credentials", writeCreds(t, 0o600, fullCreds()), storeZip(t, "0.6.0"))
	if err == nil {
		t.Fatal("want an error")
	}
	assertNoSecrets(t, out)
	if !strings.Contains(out, "cws-auth") || !strings.Contains(out, "7 days") {
		t.Fatalf("output:\n%s", out)
	}
}

// A publish the store refuses fails the command.
func TestCWSPublishRefused(t *testing.T) {
	f := &fakeCWS{publishStatus: []string{"ITEM_TAKEN_DOWN"}}
	f.start(t)
	out, err := runReleaseTools(t, "cws-upload", "--credentials", writeCreds(t, 0o600, fullCreds()), "--publish", storeZip(t, "0.6.0"))
	if err == nil || !strings.Contains(out, "ITEM_TAKEN_DOWN") {
		t.Fatalf("err %v\n%s", err, out)
	}
}

func TestCWSConsentURL(t *testing.T) {
	u, err := url.Parse(cwsConsentURL("cid", "http://127.0.0.1:8765", "st"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id":     "cid",
		"redirect_uri":  "http://127.0.0.1:8765",
		"response_type": "code",
		"scope":         "https://www.googleapis.com/auth/chromewebstore",
		"access_type":   "offline",
		"prompt":        "consent",
		"state":         "st",
	} {
		if got := q.Get(k); got != want {
			t.Fatalf("%s = %q; want %q", k, got, want)
		}
	}
	if q.Has("client_secret") {
		t.Fatal("consent URL carries the client secret")
	}
}

// cws-auth catches the loopback redirect, trades the code, and saves the
// refresh token into the file (mode 0600, other fields kept) without
// printing it.
func TestCWSAuthSavesRefreshToken(t *testing.T) {
	f := &fakeCWS{}
	f.start(t)
	fields := fullCreds()
	delete(fields, "refresh_token")
	creds := writeCreds(t, 0o600, fields)
	old := cwsOpenBrowser
	t.Cleanup(func() { cwsOpenBrowser = old })
	redirected := make(chan int, 1)
	cwsOpenBrowser = func(consent string) error {
		u, _ := url.Parse(consent)
		q := u.Query()
		if q.Get("scope") != cwsScope || !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") {
			t.Errorf("consent URL %s", consent)
		}
		go func() {
			// A request with the wrong state is refused.
			if resp, err := http.Get(q.Get("redirect_uri") + "/?code=test-code&state=wrong"); err == nil {
				resp.Body.Close()
			}
			resp, err := http.Get(q.Get("redirect_uri") + "/?code=test-code&state=" + url.QueryEscape(q.Get("state")))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			redirected <- resp.StatusCode
		}()
		return nil
	}
	out, err := runReleaseTools(t, "cws-auth", "--credentials", creds, "--port", "0", "--timeout", "10s")
	if err != nil {
		t.Fatal(out)
	}
	assertNoSecrets(t, out)
	fi, err := os.Stat(creds)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %04o", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(creds)
	var saved map[string]string
	json.Unmarshal(b, &saved)
	if saved["refresh_token"] != "new-refresh-token-55aa" || saved["item_id"] != "testitem" || saved["client_secret"] != testClientSecret {
		t.Fatalf("saved fields: item_id %q, refresh token saved %v", saved["item_id"], saved["refresh_token"] != "")
	}
	select {
	case code := <-redirected:
		if code != http.StatusOK {
			t.Fatalf("redirect answered %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the redirect never finished")
	}
}

func TestManifestVersionGreater(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.6.0", "0.5.0", true},
		{"0.5.0", "0.5.0", false},
		{"0.5", "0.5.0", false},
		{"0.5.0.1", "0.5.0", true},
		{"0.10.0", "0.9.9", true},
		{"0.4.9", "0.5.0", false},
		{"1.0", "junk", true},
	} {
		if got := manifestVersionGreater(tc.a, tc.b); got != tc.want {
			t.Errorf("manifestVersionGreater(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// releaseRepo makes a git repo holding the real Makefile with dist,
// checksums and store replaced by fakes, a bare remote with main pushed,
// and fake gh and tincan commands that log their arguments. storeRecipe is
// the fake store recipe.
func releaseRepo(t *testing.T, storeRecipe string) (repo, remote, logFile string, env []string) {
	t.Helper()
	for _, tool := range []string{"make", "git", "sh", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	mk, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	repo, remote = filepath.Join(dir, "repo"), filepath.Join(dir, "remote.git")
	bin := filepath.Join(dir, "bin")
	logFile = filepath.Join(dir, "calls.log")
	os.MkdirAll(repo, 0o755)
	os.MkdirAll(bin, 0o755)
	fakes := "\n# test fakes\n" +
		"extension:\n\tmkdir -p dist\n" +
		"dist:\n\tmkdir -p dist && for f in tincan_darwin_amd64 tincan_darwin_arm64 tincan_linux_amd64 tincan_linux_arm64 tincan-history-extension.zip; do echo $$f > dist/$$f; done\n" +
		"checksums:\n\tcd dist && shasum -a 256 tincan_* > checksums.txt\n" +
		"store:\n\t" + storeRecipe + "\n"
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), append(mk, fakes...), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("dist/\n"), 0o644)
	for _, name := range []string{"gh", "faketincan"} {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> " + logFile + "\n"
		os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755)
	}
	env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	)
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	git(dir, "init", "-q", "--bare", remote)
	git(repo, "init", "-q", "-b", "main")
	git(repo, "add", ".")
	git(repo, "commit", "-q", "-m", "init")
	git(repo, "push", "-q", remote, "main")
	return repo, remote, logFile, env
}

func runRelease(t *testing.T, repo string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("make", append([]string{"-s", "release"}, args...)...)
	cmd.Dir, cmd.Env = repo, env
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func tagExists(t *testing.T, env []string, dir, ref string) bool {
	cmd := exec.Command("git", "rev-parse", "-q", "--verify", ref)
	cmd.Dir, cmd.Env = dir, env
	return cmd.Run() == nil
}

// DRY_RUN=1 prints every step in order and runs none of them.
func TestMakeReleaseDryRun(t *testing.T) {
	repo, remote, logFile, env := releaseRepo(t, "false")
	notes := filepath.Join(t.TempDir(), "NOTES.md")
	os.WriteFile(notes, []byte("notes\n"), 0o644)
	out, err := runRelease(t, repo, env, "VERSION=0.0.0", "DRY_RUN=1", "NOTES="+notes,
		"RELEASE_REMOTE="+remote, "RELEASE_TINCAN=faketincan")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var steps []string
	for line := range strings.SplitSeq(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "+ "); ok {
			fields := strings.Fields(rest)
			if strings.HasSuffix(fields[0], "make") {
				fields[0] = "make"
			}
			steps = append(steps, strings.Join(fields, " "))
		}
	}
	want := []string{
		"git tag -a v0.0.0 -m v0.0.0",
		"make dist VERSION=0.0.0",
		"make sign-mac notarize-mac VERSION=0.0.0",
		"make checksums",
		"sh -c cd dist && shasum -a 256 -c checksums.txt",
		"make store",
		"faketincan release-tools cws-upload --dry-run dist/tincan-history-extension-store.zip",
		"git push " + remote + " refs/tags/v0.0.0",
		"gh release create v0.0.0 --repo mvanhorn/agent-tincan --verify-tag --title v0.0.0 --notes-file " + notes +
			" dist/checksums.txt dist/tincan-history-extension.zip dist/tincan_darwin_amd64 dist/tincan_darwin_arm64 dist/tincan_linux_amd64 dist/tincan_linux_arm64",
		"faketincan release-tools cws-upload --publish dist/tincan-history-extension-store.zip",
	}
	if strings.Join(steps, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps:\n%s\n\nwant:\n%s\n\noutput:\n%s", strings.Join(steps, "\n"), strings.Join(want, "\n"), out)
	}
	if strings.Contains(out, "would stop here") {
		t.Fatalf("a check failed on a clean repo:\n%s", out)
	}
	if tagExists(t, env, repo, "refs/tags/v0.0.0") || tagExists(t, env, remote, "refs/tags/v0.0.0") {
		t.Fatal("dry run created a tag")
	}
	if _, err := os.Stat(logFile); err == nil {
		t.Fatal("dry run ran gh or the release tools")
	}
	if _, err := os.Stat(filepath.Join(repo, "dist")); err == nil {
		t.Fatal("dry run built dist")
	}
}

// A real run tags, builds, pushes, releases and uploads, in that order.
func TestMakeReleaseRuns(t *testing.T) {
	repo, remote, logFile, env := releaseRepo(t, "echo store > dist/tincan-history-extension-store.zip")
	notes := filepath.Join(t.TempDir(), "NOTES.md")
	os.WriteFile(notes, []byte("notes\n"), 0o644)
	out, err := runRelease(t, repo, env, "VERSION=0.0.1-rc1", "NOTES="+notes, "SIGN=0",
		"RELEASE_REMOTE="+remote, "RELEASE_TINCAN=faketincan")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !tagExists(t, env, remote, "refs/tags/v0.0.1-rc1") {
		t.Fatalf("tag not pushed:\n%s", out)
	}
	b, _ := os.ReadFile(logFile)
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) != 3 ||
		!strings.HasPrefix(calls[0], "faketincan release-tools cws-upload --dry-run") ||
		!strings.HasPrefix(calls[1], "gh release create v0.0.1-rc1") || !strings.Contains(calls[1], "--prerelease") ||
		!strings.HasPrefix(calls[2], "faketincan release-tools cws-upload --publish") {
		t.Fatalf("calls:\n%s", b)
	}
	// A second run refuses the existing tag.
	if out, err := runRelease(t, repo, env, "VERSION=0.0.1-rc1", "NOTES="+notes, "SIGN=0",
		"RELEASE_REMOTE="+remote, "RELEASE_TINCAN=faketincan"); err == nil || !strings.Contains(out, "already exists") {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
}

// A failure before the push deletes the local tag and pushes nothing.
func TestMakeReleaseFailureDeletesLocalTag(t *testing.T) {
	repo, remote, logFile, env := releaseRepo(t, "false")
	notes := filepath.Join(t.TempDir(), "NOTES.md")
	os.WriteFile(notes, []byte("notes\n"), 0o644)
	out, err := runRelease(t, repo, env, "VERSION=0.0.2", "NOTES="+notes, "SIGN=0",
		"RELEASE_REMOTE="+remote, "RELEASE_TINCAN=faketincan")
	if err == nil {
		t.Fatalf("release with a failing store step succeeded:\n%s", out)
	}
	if tagExists(t, env, repo, "refs/tags/v0.0.2") || tagExists(t, env, remote, "refs/tags/v0.0.2") {
		t.Fatalf("tag left behind:\n%s", out)
	}
	if !strings.Contains(out, "deleting the local tag") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := os.Stat(logFile); err == nil {
		t.Fatal("gh or the release tools ran after a failure")
	}
}

// The checks stop a real run: VERSION is required and must be semver, the
// notes must exist, the tree must be clean, and HEAD must be the remote's
// main.
func TestMakeReleaseChecks(t *testing.T) {
	repo, remote, _, env := releaseRepo(t, "false")
	notes := filepath.Join(t.TempDir(), "NOTES.md")
	os.WriteFile(notes, []byte("notes\n"), 0o644)
	base := []string{"SIGN=0", "RELEASE_REMOTE=" + remote, "RELEASE_TINCAN=faketincan"}
	for _, tc := range []struct {
		name  string
		args  []string
		setup func()
		want  string
	}{
		{"no version", []string{"NOTES=" + notes}, nil, "VERSION=x.y.z is required"},
		{"leading v", []string{"VERSION=v1.0.0", "NOTES=" + notes}, nil, "VERSION must be x.y.z"},
		{"no notes", []string{"VERSION=1.0.0"}, nil, "NOTES=<file> is required"},
		{"missing notes", []string{"VERSION=1.0.0", "NOTES=/nonexistent/notes"}, nil, "does not exist"},
		{"dirty tree", []string{"VERSION=1.0.0", "NOTES=" + notes}, func() {
			os.WriteFile(filepath.Join(repo, "stray"), []byte("x"), 0o644)
		}, "not clean"},
		{"not main", []string{"VERSION=1.0.0", "NOTES=" + notes}, func() {
			os.Remove(filepath.Join(repo, "stray"))
			cmd := exec.Command("git", "commit", "-q", "--allow-empty", "-m", "ahead")
			cmd.Dir, cmd.Env = repo, env
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, b)
			}
		}, "HEAD is not main"},
	} {
		if tc.setup != nil {
			tc.setup()
		}
		out, err := runRelease(t, repo, env, append(tc.args, base...)...)
		if err == nil || !strings.Contains(out, tc.want) {
			t.Fatalf("%s: %v\n%s", tc.name, err, out)
		}
		if tagExists(t, env, repo, "refs/tags/v1.0.0") {
			t.Fatalf("%s: a failed check left a tag", tc.name)
		}
	}
}
