package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Chrome Web Store endpoints. Tests point them at a fake server.
var (
	cwsAPIBase  = "https://www.googleapis.com"
	cwsTokenURL = "https://oauth2.googleapis.com/token"
	cwsAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	// cwsOpenBrowser opens the consent URL; tests replace it.
	cwsOpenBrowser = openBrowser
	// cwsPollInterval spaces the status checks while an upload is still
	// being processed.
	cwsPollInterval = 5 * time.Second
)

const (
	cwsScope = "https://www.googleapis.com/auth/chromewebstore"
	// cwsCredentialsEnv overrides the credentials file path.
	cwsCredentialsEnv = "TINCAN_CWS_CREDENTIALS"
)

// releaseToolsCmd holds the maintainer's release helpers. It is hidden:
// users never need it, and make release calls it.
func releaseToolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "release-tools",
		Short:  "Maintainer release helpers used by make release (not for users)",
		Hidden: true,
	}
	cmd.AddCommand(cwsAuthCmd(), cwsUploadCmd())
	return cmd
}

// defaultCWSCredentials is the credentials file: $TINCAN_CWS_CREDENTIALS, or
// ~/.config/tincan-release/cws-oauth.json.
func defaultCWSCredentials() string {
	if p := os.Getenv(cwsCredentialsEnv); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "tincan-release", "cws-oauth.json")
	}
	return filepath.Join(home, ".config", "tincan-release", "cws-oauth.json")
}

// cwsCreds is the credentials file. raw keeps every field so saving a new
// refresh token leaves the rest of the file as it was.
type cwsCreds struct {
	path         string
	raw          map[string]any
	ClientID     string
	ClientSecret string
	RefreshToken string
	ItemID       string
}

// loadCWSCreds reads the credentials file, refusing one that anyone but its
// owner can read or write, or that sits in a directory others can open
// (secret files live at 0600 in 0700 directories; cws-auth writes a new
// refresh token there).
func loadCWSCreds(path string) (*cwsCreds, error) {
	dir := filepath.Dir(path)
	di, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("chrome web store credentials: %w (see docs/chrome-web-store.md)", err)
	}
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("the chrome web store credentials directory %s has mode %04o; it must be private to you: chmod 700 %s", dir, perm, dir)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("chrome web store credentials: %w (see docs/chrome-web-store.md)", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("chrome web store credentials %s have mode %04o; they must be readable only by you: chmod 600 %s", path, perm, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("chrome web store credentials: %w", err)
	}
	raw := map[string]any{}
	if err := json.Unmarshal(b, &raw); err != nil {
		// The JSON error can quote the file's bytes; name the file only.
		return nil, fmt.Errorf("chrome web store credentials %s are not valid JSON", path)
	}
	str := func(k string) string { s, _ := raw[k].(string); return s }
	return &cwsCreds{
		path:         path,
		raw:          raw,
		ClientID:     str("client_id"),
		ClientSecret: str("client_secret"),
		RefreshToken: str("refresh_token"),
		ItemID:       str("item_id"),
	}, nil
}

// require names every listed field the file lacks.
func (c *cwsCreds) require(fields ...string) error {
	var missing []string
	for _, f := range fields {
		if s, _ := c.raw[f].(string); s == "" {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	hint := ""
	if len(missing) == 1 && missing[0] == "refresh_token" {
		hint = "; run tincan release-tools cws-auth"
	}
	return fmt.Errorf("chrome web store credentials %s lack %s%s", c.path, strings.Join(missing, ", "), hint)
}

// saveRefreshToken writes the file back with the new refresh token, mode
// 0600, by renaming a temporary file over it.
func (c *cwsCreds) saveRefreshToken(tok string) error {
	c.raw["refresh_token"] = tok
	c.RefreshToken = tok
	b, err := json.MarshalIndent(c.raw, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(c.path), ".cws-oauth-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// redact removes every secret the credentials hold from s, so an error body
// that echoes one never reaches the terminal.
func (c *cwsCreds) redact(s string, extra ...string) string {
	for _, secret := range append([]string{c.ClientSecret, c.RefreshToken}, extra...) {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

// cwsConsentURL is the Google consent page URL for the Web Store scope,
// asking for offline access so the answer carries a refresh token.
func cwsConsentURL(clientID, redirect, state string) string {
	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirect},
		"response_type": {"code"},
		"scope":         {cwsScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return cwsAuthURL + "?" + q.Encode()
}

func cwsAuthCmd() *cobra.Command {
	var credsPath string
	var port int
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "cws-auth",
		Short: "Authorize the Chrome Web Store upload once and save a refresh token",
		Long: `Open Google's consent page for the Chrome Web Store scope, catch the answer
on a loopback address, and save the refresh token into the credentials file
(default ~/.config/tincan-release/cws-oauth.json, or $TINCAN_CWS_CREDENTIALS).
The file must hold client_id and client_secret and be mode 0600.
Rerun it when cws-upload reports an expired or revoked token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return cwsAuth(ctx, credsPath, port, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&credsPath, "credentials", defaultCWSCredentials(), "credentials file (env "+cwsCredentialsEnv+")")
	cmd.Flags().IntVar(&port, "port", 8765, "loopback port for the redirect (0 picks a free one)")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait for the consent")
	return cmd
}

func cwsAuth(ctx context.Context, credsPath string, port int, out io.Writer) error {
	creds, err := loadCWSCreds(credsPath)
	if err != nil {
		return err
	}
	if err := creds.require("client_id", "client_secret"); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listen for the consent redirect: %w", err)
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return err
	}
	state := hex.EncodeToString(stateBytes)

	done := make(chan error, 1)
	finish := func(err error) {
		select {
		case done <- err:
		default:
		}
	}
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("state") != state {
				http.Error(w, "state mismatch", http.StatusBadRequest)
				return
			}
			if e := q.Get("error"); e != "" {
				http.Error(w, "authorization refused: "+e, http.StatusBadRequest)
				finish(fmt.Errorf("authorization refused: %s", e))
				return
			}
			code := q.Get("code")
			if code == "" {
				http.Error(w, "no code", http.StatusBadRequest)
				return
			}
			tok, err := cwsExchangeCode(r.Context(), creds, code, redirect)
			if err == nil {
				err = creds.saveRefreshToken(tok)
			}
			if err != nil {
				http.Error(w, "tincan: authorization failed; see the terminal", http.StatusInternalServerError)
				finish(err)
				return
			}
			fmt.Fprintln(w, "tincan uploader authorized, you can close this tab")
			finish(nil)
		}),
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			finish(err)
		}
	}()
	// Shutdown, not Close, so the browser still gets the answer the handler
	// is writing when it reports success.
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		if srv.Shutdown(sctx) != nil {
			srv.Close()
		}
	}()

	consent := cwsConsentURL(creds.ClientID, redirect, state)
	fmt.Fprintf(out, "Open this URL, sign in with the publisher account and allow access:\n%s\n", consent)
	if err := cwsOpenBrowser(consent); err != nil {
		fmt.Fprintf(out, "(could not open a browser: %v)\n", err)
	}
	select {
	case err := <-done:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return fmt.Errorf("no consent before the timeout: %w", ctx.Err())
	}
	fmt.Fprintf(out, "authorized; refresh token saved to %s\n", creds.path)
	return nil
}

// cwsExchangeCode trades the consent code for tokens and returns the
// refresh token.
func cwsExchangeCode(ctx context.Context, creds *cwsCreds, code, redirect string) (string, error) {
	var tok struct {
		RefreshToken string `json:"refresh_token"`
	}
	err := cwsTokenCall(ctx, creds, url.Values{
		"code":          {code},
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
		"redirect_uri":  {redirect},
		"grant_type":    {"authorization_code"},
	}, &tok, code)
	if err != nil {
		return "", err
	}
	if tok.RefreshToken == "" {
		return "", errors.New("google returned no refresh token; remove the app's access at https://myaccount.google.com/permissions and rerun cws-auth")
	}
	return tok.RefreshToken, nil
}

// cwsAccessToken trades the refresh token for an access token.
func cwsAccessToken(ctx context.Context, creds *cwsCreds) (string, error) {
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	err := cwsTokenCall(ctx, creds, url.Values{
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
		"refresh_token": {creds.RefreshToken},
		"grant_type":    {"refresh_token"},
	}, &tok)
	if err != nil {
		if strings.Contains(err.Error(), "invalid_grant") {
			return "", fmt.Errorf("%w; the refresh token expired or was revoked (an OAuth app in testing mode issues tokens that last 7 days): run tincan release-tools cws-auth", err)
		}
		return "", err
	}
	if tok.AccessToken == "" {
		return "", errors.New("google token endpoint returned no access token")
	}
	return tok.AccessToken, nil
}

func cwsTokenCall(ctx context.Context, creds *cwsCreds, form url.Values, into any, extraSecrets ...string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cwsTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cwsHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("google token endpoint: %s", creds.redact(err.Error(), extraSecrets...))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("google token endpoint: HTTP %d: %s", resp.StatusCode, creds.redact(snippet(body), extraSecrets...))
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("google token endpoint: unreadable answer (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// cwsHTTP carries every Google call. Uploads of a small zip finish well
// inside the timeout.
var cwsHTTP = &http.Client{Timeout: 5 * time.Minute}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 1500 {
		s = s[:1500] + "..."
	}
	return s
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", u).Start()
	case "linux":
		return exec.Command("xdg-open", u).Start()
	}
	return errors.New("unsupported platform")
}

func cwsUploadCmd() *cobra.Command {
	var credsPath string
	var publish, publishOnly, dryRun bool
	cmd := &cobra.Command{
		Use:   "cws-upload <store zip> | --publish-only",
		Short: "Upload the store zip to the Chrome Web Store, and publish it with --publish",
		Long: `Upload the Web Store zip (make store) to the item named by item_id in the
credentials file, then with --publish submit it for publishing. Nothing is
uploaded or published when the store already has the zip's manifest
version or a newer one; a draft of that version that was uploaded but not
published is published when the store reports the published version.
--publish-only publishes the current draft without uploading. --dry-run
checks the credentials, the access token and the store's versions, and
changes nothing.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if publishOnly {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if publishOnly {
				return cwsPublishOnly(cmd.Context(), credsPath, dryRun, cmd.OutOrStdout())
			}
			return cwsUpload(cmd.Context(), credsPath, args[0], publish, dryRun, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&credsPath, "credentials", defaultCWSCredentials(), "credentials file (env "+cwsCredentialsEnv+")")
	cmd.Flags().BoolVar(&publish, "publish", false, "publish the item after the upload")
	cmd.Flags().BoolVar(&publishOnly, "publish-only", false, "publish the store's current draft; upload nothing")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check credentials and versions; upload and publish nothing")
	return cmd
}

// cwsPublishOnly publishes the item's current draft, for a draft whose
// earlier publish failed or never ran.
func cwsPublishOnly(ctx context.Context, credsPath string, dryRun bool, out io.Writer) error {
	creds, err := loadCWSCreds(credsPath)
	if err != nil {
		return err
	}
	if err := creds.require("client_id", "client_secret", "refresh_token", "item_id"); err != nil {
		return err
	}
	token, err := cwsAccessToken(ctx, creds)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintf(out, "dry run: would publish the draft of store item %s\n", creds.ItemID)
		return nil
	}
	c := &cwsClient{creds: creds, token: token}
	return c.publish(ctx, url.PathEscape(creds.ItemID), out)
}

// cwsUpload uploads zipPath and, with publish, publishes it.
func cwsUpload(ctx context.Context, credsPath, zipPath string, publish, dryRun bool, out io.Writer) error {
	creds, err := loadCWSCreds(credsPath)
	if err != nil {
		return err
	}
	if err := creds.require("client_id", "client_secret", "refresh_token", "item_id"); err != nil {
		return err
	}
	data, err := os.ReadFile(zipPath)
	if err != nil {
		return err
	}
	version, err := zipManifestVersion(data)
	if err != nil {
		return fmt.Errorf("%s: %w", zipPath, err)
	}
	token, err := cwsAccessToken(ctx, creds)
	if err != nil {
		return err
	}
	c := &cwsClient{creds: creds, token: token}
	item := url.PathEscape(creds.ItemID)

	draftURL := cwsAPIBase + "/chromewebstore/v1.1/items/" + item + "?projection=DRAFT"

	status, err := c.call(ctx, http.MethodGet, draftURL, nil, "")
	if err != nil {
		return fmt.Errorf("store status: %w", err)
	}
	draftVersion, _ := status["crxVersion"].(string)
	state, _ := status["uploadState"].(string)
	// The published version tells an already-published zip from a draft
	// that was uploaded but never published. An API that does not support
	// the PUBLISHED projection answers 400 or 404, and the version stays
	// unknown; any other failure stops here rather than guess.
	publishedVersion := ""
	pub, err := c.call(ctx, http.MethodGet, cwsAPIBase+"/chromewebstore/v1.1/items/"+item+"?projection=PUBLISHED", nil, "")
	se, _ := errors.AsType[*cwsStatusError](err)
	switch {
	case err == nil:
		publishedVersion, _ = pub["crxVersion"].(string)
	case se != nil && (se.Code == http.StatusBadRequest || se.Code == http.StatusNotFound):
	default:
		return fmt.Errorf("store published version: %w", err)
	}
	fmt.Fprintf(out, "store item %s: draft version %s (upload state %s), published version %s; zip version %s\n",
		creds.ItemID, orUnknown(draftVersion), orUnknown(state), orUnknown(publishedVersion), version)

	// A draft of this version whose upload failed is uploaded again.
	sameDraft := draftVersion != "" && draftVersion == version && state != "FAILURE"
	switch {
	case publishedVersion != "" && !manifestVersionGreater(version, publishedVersion):
		fmt.Fprintf(out, "the store has published version %s; manifest version %s is not newer, so nothing is uploaded or published (bump extension/manifest.json to ship the extension)\n", publishedVersion, version)
		return nil
	case sameDraft && publishedVersion != "":
		// Uploaded before but not published: a publish that failed or
		// never ran. Publish it now instead of skipping it, once its
		// upload has finished.
		fmt.Fprintf(out, "version %s is already uploaded as the draft but not published\n", version)
		if !publish {
			return nil
		}
		if dryRun {
			fmt.Fprintf(out, "dry run: would publish the draft (version %s)\n", version)
			return nil
		}
		if err := c.waitUpload(ctx, draftURL, status, out); err != nil {
			return err
		}
		return c.publish(ctx, item, out)
	case draftVersion != "" && !manifestVersionGreater(version, draftVersion) && (sameDraft || draftVersion != version):
		fmt.Fprintf(out, "the store's draft already has version %s; manifest version %s is not newer, so nothing is uploaded or published (bump extension/manifest.json to ship the extension). If that draft was never published, run tincan release-tools cws-upload --publish-only\n", draftVersion, version)
		return nil
	}
	if dryRun {
		fmt.Fprintf(out, "dry run: would upload %s (version %s)", zipPath, version)
		if publish {
			fmt.Fprint(out, " and publish it")
		}
		fmt.Fprintln(out)
		return nil
	}

	up, err := c.call(ctx, http.MethodPut, cwsAPIBase+"/upload/chromewebstore/v1.1/items/"+item, data, "application/zip")
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if err := c.waitUpload(ctx, draftURL, up, out); err != nil {
		return err
	}
	fmt.Fprintf(out, "uploaded version %s\n", version)
	if !publish {
		return nil
	}
	return c.publish(ctx, item, out)
}

// waitUpload polls the draft while its upload is IN_PROGRESS and fails
// unless it ends in SUCCESS. resp is the latest upload or status answer.
func (c *cwsClient) waitUpload(ctx context.Context, draftURL string, resp map[string]any, out io.Writer) error {
	for {
		state, _ := resp["uploadState"].(string)
		switch state {
		case "SUCCESS":
			return nil
		case "IN_PROGRESS":
		default:
			errs, _ := json.Marshal(resp["itemError"])
			return fmt.Errorf("upload ended in state %s: %s", orUnknown(state), c.creds.redact(string(errs), c.token))
		}
		fmt.Fprintln(out, "upload in progress; waiting")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cwsPollInterval):
		}
		var err error
		if resp, err = c.call(ctx, http.MethodGet, draftURL, nil, ""); err != nil {
			return fmt.Errorf("store status: %w", err)
		}
	}
}

// publish submits the item's draft for publishing. OK and
// ITEM_PENDING_REVIEW count as success.
func (c *cwsClient) publish(ctx context.Context, item string, out io.Writer) error {
	creds, token := c.creds, c.token
	pub, err := c.call(ctx, http.MethodPost, cwsAPIBase+"/chromewebstore/v1.1/items/"+item+"/publish", nil, "")
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	statuses, _ := pub["status"].([]any)
	var names []string
	ok := len(statuses) > 0
	for _, s := range statuses {
		name, _ := s.(string)
		names = append(names, name)
		if name != "OK" && name != "ITEM_PENDING_REVIEW" {
			ok = false
		}
	}
	if !ok {
		details, _ := json.Marshal(pub["statusDetail"])
		return fmt.Errorf("publish refused: %s %s", strings.Join(names, ", "), creds.redact(string(details), token))
	}
	fmt.Fprintf(out, "published: %s\n", strings.Join(names, ", "))
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// cwsStatusError is a Web Store answer outside 2xx. Body is already
// redacted.
type cwsStatusError struct {
	Code int
	Body string
}

func (e *cwsStatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Body) }

type cwsClient struct {
	creds *cwsCreds
	token string
}

// call makes one Web Store API call and decodes its JSON answer.
func (c *cwsClient) call(ctx context.Context, method, u string, body []byte, ctype string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("x-goog-api-version", "2")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := cwsHTTP.Do(req)
	if err != nil {
		return nil, errors.New(c.creds.redact(err.Error(), c.token))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, &cwsStatusError{Code: resp.StatusCode, Body: c.creds.redact(snippet(b), c.token)}
	}
	m := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("unreadable answer (HTTP %d)", resp.StatusCode)
		}
	}
	return m, nil
}

// zipManifestVersion reads manifest.json's version from a store zip.
func zipManifestVersion(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("not a zip: %w", err)
	}
	f, err := zr.Open("manifest.json")
	if err != nil {
		return "", errors.New("no manifest.json in the zip")
	}
	defer f.Close()
	var m struct {
		Version string `json:"version"`
		Key     string `json:"key"`
	}
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return "", fmt.Errorf("manifest.json: %w", err)
	}
	if m.Key != "" {
		return "", errors.New(`manifest.json has a "key", which the store rejects; upload the make store zip`)
	}
	if _, ok := parseManifestVersion(m.Version); !ok {
		return "", fmt.Errorf("manifest.json version %q is not 1 to 4 dot-separated integers", m.Version)
	}
	return m.Version, nil
}

func parseManifestVersion(v string) ([]int, bool) {
	parts := strings.Split(v, ".")
	if len(parts) < 1 || len(parts) > 4 {
		return nil, false
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		nums[i] = n
	}
	return nums, true
}

// manifestVersionGreater reports whether Chrome extension version a is newer
// than b. An unparsable b counts as older, so the store decides.
func manifestVersionGreater(a, b string) bool {
	x, ok := parseManifestVersion(a)
	if !ok {
		return false
	}
	y, ok := parseManifestVersion(b)
	if !ok {
		return true
	}
	for i := range max(len(x), len(y)) {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q
		}
	}
	return false
}
