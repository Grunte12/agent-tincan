// Package notes is the notes agent: a non-model Tincan service that saves
// notes into Agent Notes and finds and reads them back, by driving the
// app's bundled agent-notes helper. A claimed add is spooled to disk
// before the library is touched and is answered only once the helper has
// returned its note id, so an add is never lost to a sleeping Mac, a
// missing library, or a crash (KTD3, KTD9).
package notes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/history"
)

// DefaultAgentName is the notes agent's Tincan name.
const DefaultAgentName = "notes"

// Defaults for the service's timing.
const (
	DefaultRetryEvery     = time.Minute
	DefaultRequestTimeout = 3 * time.Minute
)

// fromAgentTag is the tag every note added through Tincan carries (R9).
const fromAgentTag = "from-agent"

// maxExtractAttempts is how many failed extractions a request gets before
// it is answered failed (KTD6).
const maxExtractAttempts = 3

// maxReplyBytes caps every reply body.
const maxReplyBytes = 64 << 10

// replyTimeout bounds sending one reply or progress note.
const replyTimeout = 30 * time.Second

// Config is everything a notes service needs. Relay, LibraryRoot,
// AppSupportRoot, and SpoolDir are required.
type Config struct {
	// Relay is the client the service polls and replies with, as its own
	// agent identity.
	Relay *client.Relay
	// Agent is the service's agent name, used in replies (DefaultAgentName
	// when empty).
	Agent string
	// HelperPath is the agent-notes helper (DefaultHelperPath when empty).
	HelperPath string
	// LibraryRoot is the Agent Notes library the helper is run against.
	LibraryRoot string
	// AppSupportRoot is the service's own Application Support directory,
	// passed to the helper so it never picks up an in-app agent session's
	// context (KTD8). It is created if missing.
	AppSupportRoot string
	// SpoolDir holds unanswered adds, one file per request id.
	SpoolDir string
	// ReadAllowlistPath and AddAllowlistPath are the allowlist files for
	// search and read, and for add. Each is reread for every request; a
	// missing file (or an empty path) allows every joined agent.
	ReadAllowlistPath string
	AddAllowlistPath  string
	// HealthPath is where the last helper result is written for doctor
	// (AppSupportRoot/tincan-notes-health.json when empty).
	HealthPath string
	// Extractor turns free text into a request. Nil fails free text with
	// the structured form.
	Extractor Extractor
	// Hold is the long-poll hold (client.DefaultPollHold when zero).
	Hold time.Duration
	// RetryEvery is how often spooled adds are retried off the poll loop
	// (DefaultRetryEvery when zero).
	RetryEvery time.Duration
	// RequestTimeout bounds handling one request (DefaultRequestTimeout
	// when zero).
	RequestTimeout time.Duration
	// PresenceInterval is how often presence is refreshed while a request
	// is handled (client.DefaultPresenceInterval when zero).
	PresenceInterval time.Duration
	// Log receives one line per request and per error (stderr when nil).
	Log io.Writer
}

// Service is the notes agent.
type Service struct {
	relay      *client.Relay
	agent      string
	helper     *Helper
	spool      *Spool
	healthPath string
	readAllow  func() ([]string, error)
	addAllow   func() ([]string, error)
	extractor  Extractor
	hold       time.Duration
	retryEvery time.Duration
	timeout    time.Duration
	presence   time.Duration
	log        io.Writer

	mu       sync.Mutex
	inflight map[string]bool // request ids being applied, loop or timer
	// extractFails counts failed extractions per request id, so the third
	// one is answered (KTD6). It is in memory: a restart gives a request
	// fresh attempts, which only delays its answer.
	extractFails map[string]int
	// createMu runs one helper create at a time: the helper looks an
	// idempotency key up in the library as loaded at its start, so two
	// concurrent creates for one key could both write a note.
	createMu sync.Mutex
	healthMu sync.Mutex
}

// New builds a service from cfg and creates its directories.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.Relay == nil:
		return nil, errors.New("notes: no relay client")
	case cfg.LibraryRoot == "":
		return nil, errors.New("notes: no library root")
	case cfg.AppSupportRoot == "":
		return nil, errors.New("notes: no Application Support root")
	case cfg.SpoolDir == "":
		return nil, errors.New("notes: no spool dir")
	}
	for _, d := range []string{cfg.AppSupportRoot, cfg.SpoolDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("notes: %w", err)
		}
	}
	s := &Service{
		relay:      cfg.Relay,
		agent:      orDefault(cfg.Agent, DefaultAgentName),
		helper:     &Helper{Path: orDefault(cfg.HelperPath, DefaultHelperPath), LibraryRoot: cfg.LibraryRoot, AppSupportRoot: cfg.AppSupportRoot},
		spool:      &Spool{Dir: cfg.SpoolDir},
		healthPath: orDefault(cfg.HealthPath, HealthPathIn(cfg.AppSupportRoot)),
		readAllow:  allowlistAt(cfg.ReadAllowlistPath),
		addAllow:   allowlistAt(cfg.AddAllowlistPath),
		extractor:  cfg.Extractor,
		hold:       cfg.Hold,
		retryEvery: cfg.RetryEvery,
		timeout:    cfg.RequestTimeout,
		presence:   cfg.PresenceInterval,
		log:        cfg.Log,
		inflight:   map[string]bool{},

		extractFails: map[string]int{},
	}
	if s.retryEvery <= 0 {
		s.retryEvery = DefaultRetryEvery
	}
	if s.timeout <= 0 {
		s.timeout = DefaultRequestTimeout
	}
	return s, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func allowlistAt(path string) func() ([]string, error) {
	if path == "" {
		return history.StaticAllowlist(history.DefaultAllowlist...)
	}
	return history.FileAllowlist(path)
}

func (s *Service) logf(format string, args ...any) {
	w := s.log
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "tincan notes: "+format+"\n", args...)
}

// Run drains the spool once in the background, retries spooled adds every
// RetryEvery, and polls and handles requests until ctx is cancelled. The
// drain and retries never hold up polling (KTD9).
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() {
		s.RetrySpooled(ctx)
		t := time.NewTicker(s.retryEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.RetrySpooled(ctx)
			}
		}
	})
	err := history.RunPolling(ctx, s.PollOnce, s.logf)
	wg.Wait()
	return err
}

// PollOnce waits up to the hold for requests and handles each serially.
func (s *Service) PollOnce(ctx context.Context) (int, error) {
	return history.PollAndHandle(ctx, s.relay, s.hold, "notes-serve", s.handleSafely)
}

// RetrySpooled makes one attempt at every spooled add not already being
// applied, and returns how many it tried.
func (s *Service) RetrySpooled(ctx context.Context) int {
	entries, err := s.spool.List()
	if err != nil {
		s.logf("spool: %v", err)
	}
	n := 0
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		if !s.begin(e.Request.ID) {
			continue
		}
		n++
		func() {
			defer s.end(e.Request.ID)
			actx, cancel := context.WithTimeout(ctx, s.timeout)
			defer cancel()
			s.applyAdd(actx, e, false)
		}()
	}
	return n
}

// begin marks id as being applied; false means it already is.
func (s *Service) begin(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[id] {
		return false
	}
	s.inflight[id] = true
	return true
}

func (s *Service) end(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
}

// handleSafely handles one request so that nothing it does, including a
// panic, stops the loop, and keeps the service online meanwhile.
func (s *Service) handleSafely(ctx context.Context, req envelope.Request) {
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer cancel()
	defer s.relay.KeepPresence(hctx, client.Presence{Every: s.presence, Logf: s.logf})()
	defer func() {
		if p := recover(); p != nil {
			s.logf("request %s: panic: %v", req.ID, p)
			s.reply(hctx, req, "The notes agent hit an internal error handling this request.", envelope.StatusFailed)
		}
	}()
	s.Handle(hctx, req)
}

// Handle claims and answers one request. An add that cannot be applied
// yet is left spooled and unanswered.
func (s *Service) Handle(ctx context.Context, req envelope.Request) {
	if _, err := s.relay.Claim(ctx, req.ID); err != nil {
		s.logf("request %s from %s: claim failed: %v", req.ID, req.From, err)
		return
	}

	// A redelivered add that is already spooled goes straight to the
	// helper: the idempotency key makes that return the same note.
	if e, ok, err := s.spool.Get(req.ID); err != nil {
		s.logf("request %s: spool entry unreadable, handling it afresh: %v", req.ID, err)
	} else if ok {
		if !s.begin(req.ID) {
			s.logf("request %s: redelivered while a retry is applying it", req.ID)
			return
		}
		defer s.end(req.ID)
		s.logf("request %s from %s: redelivered add, applying from the spool", req.ID, req.From)
		s.applyAdd(ctx, e, true)
		return
	}

	// 1. Access, from relay-set fields only, before the body is parsed or
	// shown to any extractor: the chain must pass at least one allowlist.
	if reason := history.ChainDenied(s.eitherAllow, req, s.agent, "read or add notes", s.logf); reason != "" {
		s.logf("request %s from %s (chain %v): declined: %s", req.ID, req.From, req.Chain, reason)
		s.reply(ctx, req, reason, envelope.StatusDeclined)
		return
	}

	// 2. The request: structured JSON parsed in Go, or free text for the
	// extractor.
	r, ok := s.parse(ctx, req)
	if !ok {
		return
	}

	// 3. The operation's own allowlist.
	allow, what, list := s.readAllow, "read notes", s.agent
	if r.Op == OpAdd {
		allow, what, list = s.addAllow, "add notes", s.agent+" add"
	}
	if reason := history.ChainDenied(allow, req, list, what, s.logf); reason != "" {
		s.logf("request %s from %s (chain %v): %s declined: %s", req.ID, req.From, req.Chain, r.Op, reason)
		s.reply(ctx, req, reason, envelope.StatusDeclined)
		return
	}

	switch r.Op {
	case OpAdd:
		s.add(ctx, req, r)
	case OpSearch:
		s.search(ctx, req, r)
	case OpRead:
		s.read(ctx, req, r)
	}
}

// eitherAllow is the union of the two allowlists.
func (s *Service) eitherAllow() ([]string, error) {
	var out []string
	for _, f := range []func() ([]string, error){s.readAllow, s.addAllow} {
		names, err := f()
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// parse turns req's body into a valid Request, replying failed when it
// cannot.
func (s *Service) parse(ctx context.Context, req envelope.Request) (Request, bool) {
	if raw, ok := structuredBody(req.Body); ok {
		r, err := ParseStructured(raw)
		if err != nil {
			s.logf("request %s from %s: structured request rejected: %v", req.ID, req.From, err)
			s.reply(ctx, req, fmt.Sprintf("The note: request was not valid (%v), so the notes agent did not run it. %s", err, StructuredHelp), envelope.StatusFailed)
			return Request{}, false
		}
		s.logf("request %s from %s: structured %s, no model step", req.ID, req.From, r.Op)
		return r, true
	}
	if s.extractor == nil {
		s.logf("request %s from %s: free text and no extractor", req.ID, req.From)
		s.reply(ctx, req, "The notes agent only takes structured requests right now. "+StructuredHelp, envelope.StatusFailed)
		return Request{}, false
	}
	r, err := s.extractor.Extract(ctx, req.Body)
	switch {
	case errors.Is(err, ErrUnclearRequest):
		s.forgetExtractFails(req.ID)
		s.logf("request %s from %s: unclear request: %v", req.ID, req.From, err)
		s.reply(ctx, req, "I could not tell whether to save, search, or read a note, so nothing was done. "+StructuredHelp, envelope.StatusFailed)
		return Request{}, false
	case err != nil:
		// The service never guesses the operation (KTD6): the request is
		// left claimed and unanswered, and the relay redelivers it once the
		// claim lease runs out. The third failure is answered.
		n := s.countExtractFail(req.ID)
		if n < maxExtractAttempts {
			s.logf("request %s from %s: extractor failed (attempt %d of %d), leaving it for redelivery: %v", req.ID, req.From, n, maxExtractAttempts, err)
			return Request{}, false
		}
		s.forgetExtractFails(req.ID)
		s.logf("request %s from %s: extractor failed (attempt %d of %d), answering failed: %v", req.ID, req.From, n, maxExtractAttempts, err)
		s.reply(ctx, req, "The notes agent could not work out this free-text request (its request step failed several times), so nothing was done. "+StructuredHelp, envelope.StatusFailed)
		return Request{}, false
	}
	s.forgetExtractFails(req.ID)
	// The extractor never writes an add's body: the sender's text is saved
	// verbatim (R4).
	if r.Op == OpAdd {
		r.Body = req.Body
	}
	if r.Op == OpSearch && r.Count == 0 {
		r.Count = DefaultSearchCount
	}
	r.Count = min(r.Count, MaxSearchCount)
	if err := Validate(r); err != nil {
		s.logf("request %s from %s: extracted request not valid: %v", req.ID, req.From, err)
		s.reply(ctx, req, fmt.Sprintf("The request could not be carried out (%v), so nothing was done. %s", err, StructuredHelp), envelope.StatusFailed)
		return Request{}, false
	}
	return r, true
}

// countExtractFail records a failed extraction of id and returns how many
// there have been.
func (s *Service) countExtractFail(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extractFails[id]++
	return s.extractFails[id]
}

func (s *Service) forgetExtractFails(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.extractFails, id)
}

// add spools r and applies it. Validation failures are answered before
// anything is spooled or written.
func (s *Service) add(ctx context.Context, req envelope.Request, r Request) {
	if err := Validate(r); err != nil {
		s.reply(ctx, req, fmt.Sprintf("The note was not saved: %v. Nothing was written.", err), envelope.StatusFailed)
		return
	}
	if !s.begin(req.ID) {
		return
	}
	defer s.end(req.ID)
	e := SpoolEntry{Request: req, Note: r, SpooledAt: time.Now().UTC()}
	if err := s.spool.Put(e); err != nil {
		// Without a durable entry the library is not touched; the relay
		// redelivers once the claim lease runs out.
		s.logf("request %s: spool write failed, leaving it for redelivery: %v", req.ID, err)
		s.progress(ctx, req, "The notes agent could not record this add on disk yet; it will be retried.")
		return
	}
	s.applyAdd(ctx, e, true)
}

// applyAdd calls the helper for a spooled add and replies. The caller
// holds the in-flight mark for the request. fresh is true when the request
// was just claimed from the poll loop, which is when a progress note can
// reach the asker.
func (s *Service) applyAdd(ctx context.Context, e SpoolEntry, fresh bool) {
	req := e.Request
	tags := slices.Clone(e.Note.Tags)
	if !slices.Contains(tags, fromAgentTag) {
		tags = append(tags, fromAgentTag)
	}
	props := map[string]string{
		"remote.agent":    req.From,
		"remote.trace-id": req.TraceID,
		"remote.via":      "tincan",
	}
	s.createMu.Lock()
	res, err := s.helper.Create(ctx, e.Note.Title, e.Note.Body, tags, props, "tincan:"+req.ID)
	s.createMu.Unlock()
	if err == nil && res.NoIdempotency {
		// The note was written, so the asker gets its id, but this helper
		// cannot dedupe a redelivered add: tell the owner loudly.
		s.logf("request %s: WARNING %s: the Agent Notes helper ignored the idempotency key; a redelivered add could be saved twice. %s", req.ID, codeHelperTooOld, helperTooOldMessage)
		s.recordHealth("create", req.ID, &HelperError{Code: codeHelperTooOld, Message: helperTooOldMessage})
	} else {
		s.recordHealth("create", req.ID, err)
	}
	if err != nil {
		if IsPermanent(err) {
			s.logf("request %s from %s: add rejected by the helper: %v", req.ID, req.From, err)
			s.sendFinal(ctx, e, fmt.Sprintf("The note was not saved: the Agent Notes helper rejected it (%s). Nothing was written.", helperReason(err)), envelope.StatusFailed)
			return
		}
		e.Attempts++
		e.LastError = err.Error()
		if perr := s.spool.Put(e); perr != nil {
			s.logf("request %s: spool update failed: %v", req.ID, perr)
		}
		s.logf("request %s from %s: add not applied yet (attempt %d), kept for retry: %v", req.ID, req.From, e.Attempts, err)
		if fresh {
			code := errorCode(err)
			s.progress(ctx, req, fmt.Sprintf("Not saved to Agent Notes yet (helper error %s). The add is kept on the notes Mac and will be retried; the reply will carry the note id.", code))
		}
		return
	}
	s.logf("request %s from %s: add applied as note %s (existed %v, state %s)", req.ID, req.From, res.Note.Summary.ID, res.Existed, res.Note.Summary.State)
	s.sendFinal(ctx, e, addedReply(res), envelope.StatusAnswered)
}

// sendFinal sends the final reply for a spooled add and clears its entry,
// unless the relay could not be reached: then the entry stays for a
// retry, which calls the helper again and gets the same note. A request
// the relay has already closed (expired, cancelled) is logged and
// dropped, never retried forever.
func (s *Service) sendFinal(ctx context.Context, e SpoolEntry, body string, status envelope.Status) {
	err := s.replyErr(ctx, e.Request, body, status)
	switch {
	case err == nil:
	case client.IsStatus(err, http.StatusConflict), client.IsStatus(err, http.StatusNotFound):
		s.logf("request %s: reply failed, the request is no longer open, dropping it: %v", e.Request.ID, err)
	default:
		s.logf("request %s: reply failed, keeping it spooled to retry: %v", e.Request.ID, err)
		return
	}
	if err := s.spool.Remove(e.Request.ID); err != nil {
		s.logf("request %s: spool remove failed: %v", e.Request.ID, err)
	}
}

func (s *Service) search(ctx context.Context, req envelope.Request, r Request) {
	found, err := s.helper.Search(ctx, r.Query)
	s.recordHealth("search", req.ID, err)
	if err != nil {
		s.logf("request %s: search: %v", req.ID, err)
		s.reply(ctx, req, helperFailedReply(err), envelope.StatusFailed)
		return
	}
	var active []NoteSummary
	for _, n := range found {
		if n.State == StateActive {
			active = append(active, n)
		}
		if len(active) == r.Count {
			break
		}
	}
	s.logf("request %s from %s: search answered (%d of %d matches)", req.ID, req.From, len(active), len(found))
	s.reply(ctx, req, searchReply(r.Query, active), envelope.StatusAnswered)
}

func (s *Service) read(ctx context.Context, req envelope.Request, r Request) {
	n, err := s.helper.Read(ctx, r.ID)
	s.recordHealth("read", req.ID, err)
	if errors.Is(err, ErrNoteNotFound) || (err == nil && n.Summary.State != StateActive) {
		s.logf("request %s from %s: read %s: not found", req.ID, req.From, r.ID)
		s.reply(ctx, req, notFoundReply(r.ID), envelope.StatusFailed)
		return
	}
	if err != nil {
		s.logf("request %s: read: %v", req.ID, err)
		s.reply(ctx, req, helperFailedReply(err), envelope.StatusFailed)
		return
	}
	s.logf("request %s from %s: read %s answered", req.ID, req.From, r.ID)
	s.reply(ctx, req, readReply(n), envelope.StatusAnswered)
}

// reply sends on a context detached from ctx's deadline, so a request that
// ran out of time still gets its answer.
func (s *Service) reply(ctx context.Context, req envelope.Request, body string, status envelope.Status) {
	if err := s.replyErr(ctx, req, body, status); err != nil {
		s.logf("request %s: reply failed: %v", req.ID, err)
	}
}

func (s *Service) replyErr(ctx context.Context, req envelope.Request, body string, status envelope.Status) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyTimeout)
	defer cancel()
	_, err := s.relay.Reply(rctx, req.ID, body, status)
	return err
}

// progress posts a progress note to the asker; a failure is only logged.
func (s *Service) progress(ctx context.Context, req envelope.Request, note string) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyTimeout)
	defer cancel()
	if len(note) > envelope.MaxProgressNote {
		note = capBytes(note, envelope.MaxProgressNote)
	}
	if err := s.relay.Progress(pctx, req.ID, note); err != nil {
		s.logf("request %s: progress note failed: %v", req.ID, err)
	}
}

// Health is the last helper result, written for doctor (U5) after every
// helper call.
type Health struct {
	UpdatedAt time.Time `json:"updated_at"`
	Command   string    `json:"command"`
	RequestID string    `json:"request_id,omitempty"`
	OK        bool      `json:"ok"`
	Code      string    `json:"code,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// recordHealth writes the health file; a failure is only logged.
func (s *Service) recordHealth(command, requestID string, err error) {
	h := Health{UpdatedAt: time.Now().UTC(), Command: command, RequestID: requestID, OK: err == nil}
	if err != nil {
		if he, ok := errors.AsType[*HelperError](err); ok {
			h.Code, h.Message = he.Code, he.Message
		} else if !errors.Is(err, ErrNoteNotFound) {
			h.Message = err.Error()
		} else {
			h.OK = true // a clean "no such note" is a working helper
		}
	}
	b, merr := json.MarshalIndent(h, "", "  ")
	if merr != nil {
		return
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if werr := writeFileAtomic(s.healthPath, append(b, '\n')); werr != nil {
		s.logf("health file %s: %v", s.healthPath, werr)
	}
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".health-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Reply templates. Only fields from the helper are filled in; nothing in a
// note is interpreted.

func addedReply(res CreateResult) string {
	n := res.Note.Summary
	if !res.Existed {
		return fmt.Sprintf("Saved note %q (id %s).", oneLine(n.Title), n.ID)
	}
	where := ""
	switch n.State {
	case StateActive:
	case "trashed":
		where = ", which has since been moved to the trash"
	case "archived":
		where = ", which has since been archived"
	default:
		where = fmt.Sprintf(", which is now %s", n.State)
	}
	return fmt.Sprintf("This request was already saved as note %q (id %s)%s. No new note was created.", oneLine(n.Title), n.ID, where)
}

func notFoundReply(id string) string { return fmt.Sprintf("No note with id %s was found.", id) }

func helperFailedReply(err error) string {
	return fmt.Sprintf("The notes agent could not reach the Agent Notes library right now (%s). Try again later.", helperReason(err))
}

// helperReason is err's helper code and message for a reply.
func helperReason(err error) string {
	if he, ok := errors.AsType[*HelperError](err); ok {
		if he.Message == "" {
			return he.Code
		}
		return he.Code + ": " + oneLine(capRunes(he.Message, 300))
	}
	return "helper failed"
}

func searchReply(query string, found []NoteSummary) string {
	q := oneLine(query)
	if len(found) == 0 {
		return fmt.Sprintf("No notes match %q.", q)
	}
	var b strings.Builder
	if len(found) == 1 {
		fmt.Fprintf(&b, "1 note matches %q:\n", q)
	} else {
		fmt.Fprintf(&b, "%d notes match %q:\n", len(found), q)
	}
	for i, n := range found {
		title := oneLine(n.Title)
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "\n%d. %q (id %s)\n", i+1, title, n.ID)
		if len(n.Tags) > 0 {
			fmt.Fprintf(&b, "   Tags: %s\n", oneLine(strings.Join(n.Tags, ", ")))
		}
		if sn := oneLine(n.Snippet); sn != "" {
			fmt.Fprintf(&b, "   %s\n", capRunes(sn, 300))
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if len(out) > maxReplyBytes {
		const notice = "\n(reply truncated)"
		out = capBytes(out, maxReplyBytes-len(notice)) + notice
	}
	return out
}

func readReply(n Note) string {
	var head strings.Builder
	title := oneLine(n.Summary.Title)
	if title == "" {
		title = "(untitled)"
	}
	fmt.Fprintf(&head, "Note %q (id %s)\n", title, n.Summary.ID)
	if len(n.Summary.Tags) > 0 {
		fmt.Fprintf(&head, "Tags: %s\n", oneLine(strings.Join(n.Summary.Tags, ", ")))
	}
	head.WriteString("\n")
	out := head.String() + n.Body
	if len(out) <= maxReplyBytes {
		return out
	}
	notice := fmt.Sprintf("\n\n(note truncated: the body is %d bytes and only the start fits in a reply)", len(n.Body))
	return capBytes(out, maxReplyBytes-len(notice)) + notice
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// capBytes cuts s to at most n bytes on a rune boundary.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
