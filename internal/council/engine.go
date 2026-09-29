package council

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// Relay is the part of the relay client the engine uses; *client.Relay
// implements it.
type Relay interface {
	SendEach(ctx context.Context, outs []client.Outgoing, kind envelope.Kind, parent string) (client.GroupResult, []error, error)
	Get(ctx context.Context, id string, wait time.Duration) (client.Result, error)
	Cancel(ctx context.Context, id string) error
	FetchAttachment(ctx context.Context, id string) ([]byte, client.DownloadedAttachment, error)
}

// maxBatch is the most member asks sent under one group id, the relay's
// per-group cap.
const maxBatch = 8

// maxForwardBytes caps the attachment bytes one council uploads to its
// members. A file past it is inlined when it is text and left out
// otherwise.
var maxForwardBytes int64 = 20 << 20

// pollStep is how often a stage re-checks its asks once less than a
// second is left, since relay waits are whole seconds.
const pollStep = 100 * time.Millisecond

// Stages.
const (
	StageAnswer   = "answer"
	StageReview   = "review"
	StageChairman = "chairman"
)

// Why a member is absent from a stage.
const (
	AbsentTimedOut   = "timed out"
	AbsentHeld       = "held by a gate"
	AbsentFailed     = "failed"
	AbsentDeclined   = "declined"
	AbsentNeedsInput = "needs input"
	// AbsentUnranked is an answer no valid ballot ranked.
	AbsentUnranked = "not ranked"
	// AbsentNoVerdict is a chairman whose reply had no verdict to parse.
	AbsentNoVerdict = "no verdict in reply"
)

// How a context file reached a member.
const (
	DeliveryInlined  = "inlined"
	DeliveryCut      = "inlined, cut to fit"
	DeliveryAttached = "attached"
	DeliveryLeftOut  = "left out"
)

// Engine runs a council's answer and review stages over the relay and
// scores the answers.
type Engine struct {
	Relay Relay
	// Config supplies the stage time limits.
	Config Config
	// Now and After are the clock; nil means the real one.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// Rand shuffles reviewers' labels and answer order; nil seeds one.
	Rand *rand.Rand
}

// Seat is one council member. Web members read only text, so context
// files are inlined for them and attached for the rest.
type Seat struct {
	Name string
	Web  bool
}

// Council is one council to run.
type Council struct {
	// Request is the claimed convening request: every member ask is its
	// child, and its attachments are the context sent with the question.
	Request  envelope.Request
	Question string
	Members  []Seat
	// Chairmen is the chairman failover order (Eligibility.Chairmen).
	Chairmen []string
}

// Answer is one member's answer.
type Answer struct {
	Member    string
	RequestID string
	// Body is the member's full reply; Copy is what reviewers and the
	// chairman see (see reviewerCopy).
	Body, Copy string
	Elapsed    time.Duration
}

// Review is one reviewer's ballot.
type Review struct {
	Reviewer  string
	RequestID string
	Body      string
	// Labels maps each label this reviewer was shown to the member whose
	// answer it stood for.
	Labels map[string]string
	// Ranked is the parsed ballot as members, best first, own answer
	// included as the reviewer ranked it.
	Ranked []string
	// Valid is whether the ballot counted in the tally.
	Valid bool
}

// Absence is a member missing from a stage, and why.
type Absence struct {
	Member, Stage, Reason string
	// Detail is the member's own reply text for a failure or decline.
	Detail string
}

// ContextNote says how one context file reached one member.
type ContextNote struct {
	File, Member, Delivery string
}

// Outcome is what the answer and review stages produced.
type Outcome struct {
	// State is CouncilCompleted when the answers were scored, CouncilFailed
	// without a quorum, and CouncilDeclined when nobody could be asked.
	State  CouncilState
	Reason string
	// Answers are in seat order; Standings best first.
	Answers   []Answer
	Reviews   []Review
	Standings []Standing
	Absent    []Absence
	Context   []ContextNote
	// Truncated names the members whose answers reviewers saw cut to fit.
	Truncated []string
	// Verdict is the chairman's, set when State is CouncilCompleted.
	Verdict                           Verdict
	AnswerTime, ReviewTime, ChairTime time.Duration
}

// Run runs the answer stage, then, with a quorum of answers, the review
// stage, scores the ballots, and has a chairman write the verdict. An error is returned only when ctx ends;
// every other problem is in the Outcome.
func (e *Engine) Run(ctx context.Context, c Council) (Outcome, error) {
	e.defaults()
	var out Outcome
	files, fetchNotes := e.fetchContext(ctx, c)
	out.Context = fetchNotes

	start := e.Now()
	plans, err := answerPlans(c, files, randomNonce())
	if err != nil {
		out.State, out.Reason = CouncilDeclined, err.Error()
		return out, nil
	}
	res, notes, err := e.stage(ctx, c.Request.ID, plans, e.Config.AnswerLimit)
	if err != nil {
		return out, err
	}
	out.Context = append(out.Context, notes...)
	out.AnswerTime = e.Now().Sub(start)
	for _, s := range c.Members {
		r := res[s.Name]
		if r.Status != envelope.StatusAnswered {
			out.Absent = append(out.Absent, absence(s.Name, StageAnswer, r))
			continue
		}
		out.Answers = append(out.Answers, Answer{Member: s.Name, RequestID: r.Request.ID, Body: r.Reply.Body, Copy: reviewerCopy(r.Reply.Body, s.Web), Elapsed: elapsed(r)})
	}
	if len(out.Answers) < MinMembers {
		out.State = CouncilFailed
		out.Reason = fmt.Sprintf("only %d of %d members answered in time, and a council needs at least %d", len(out.Answers), len(c.Members), MinMembers)
		return out, nil
	}

	start = e.Now()
	var inline []contextFile
	for _, f := range files {
		if f.text {
			inline = append(inline, contextFile{Name: f.name, Text: string(f.data)})
		}
	}
	nonce := randomNonce()
	cut := map[string]bool{}
	plans = nil
	for _, a := range out.Answers {
		body, rev, cutLabels, err := e.reviewFor(c.Question, inline, a.Member, out.Answers, nonce)
		if err != nil {
			out.State, out.Reason = CouncilFailed, err.Error()
			return out, nil
		}
		for _, l := range cutLabels {
			cut[rev.Labels[l]] = true
		}
		out.Reviews = append(out.Reviews, rev)
		plans = append(plans, plan{member: a.Member, out: client.Outgoing{To: a.Member, Body: body}})
	}
	res, _, err = e.stage(ctx, c.Request.ID, plans, e.Config.ReviewLimit)
	if err != nil {
		return out, err
	}
	out.ReviewTime = e.Now().Sub(start)
	for _, a := range out.Answers {
		if cut[a.Member] {
			out.Truncated = append(out.Truncated, a.Member)
		}
	}
	for i := range out.Reviews {
		rv := &out.Reviews[i]
		r := res[rv.Reviewer]
		rv.RequestID = r.Request.ID
		if r.Status != envelope.StatusAnswered {
			out.Absent = append(out.Absent, absence(rv.Reviewer, StageReview, r))
			continue
		}
		rv.Body = r.Reply.Body
		labels := make([]string, 0, len(rv.Labels))
		for l := range rv.Labels {
			labels = append(labels, l)
		}
		for _, l := range parseBallot(r.Reply.Body, labels) {
			rv.Ranked = append(rv.Ranked, rv.Labels[l])
		}
		rv.Valid = len(countedBallot(*rv)) >= minBallotSize
	}
	standings, valid := score(out.Reviews)
	if valid < MinMembers {
		out.State = CouncilFailed
		out.Reason = fmt.Sprintf("only %d valid ballots came back, and a council needs at least %d", valid, MinMembers)
		return out, nil
	}
	out.Standings = standings
	for _, a := range out.Answers {
		if !slices.ContainsFunc(standings, func(s Standing) bool { return s.Member == a.Member }) {
			out.Absent = append(out.Absent, Absence{Member: a.Member, Stage: StageReview, Reason: AbsentUnranked})
		}
	}
	out.State = CouncilCompleted
	start = e.Now()
	out.Verdict, err = e.chair(ctx, c, out)
	out.ChairTime = e.Now().Sub(start)
	return out, err
}

func (e *Engine) defaults() {
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.After == nil {
		e.After = time.After
	}
	if e.Rand == nil {
		e.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
}

func randomNonce() string { return crand.Text()[:16] }

// fetched is one of the convener's attachments.
type fetched struct {
	name string
	mime string
	data []byte
	text bool
}

// fetchContext downloads the convening request's attachments. A file that
// cannot be fetched is noted as left out for every member.
func (e *Engine) fetchContext(ctx context.Context, c Council) ([]fetched, []ContextNote) {
	var files []fetched
	var notes []ContextNote
	for _, a := range c.Request.Attachments {
		name := a.Name
		if name == "" {
			name = a.ID
		}
		data, d, err := e.Relay.FetchAttachment(ctx, a.ID)
		if err != nil {
			for _, s := range c.Members {
				notes = append(notes, ContextNote{File: name, Member: s.Name, Delivery: DeliveryLeftOut})
			}
			continue
		}
		files = append(files, fetched{name: name, mime: d.MIME, data: data, text: utf8.Valid(data) && !bytes.ContainsRune(data, 0)})
	}
	return files, notes
}

// plan is one member ask. fallback, when set, is sent instead if the
// ask's files cannot be uploaded; notes say how context reached the
// member either way.
type plan struct {
	member        string
	out           client.Outgoing
	notes         []ContextNote
	fallback      *client.Outgoing
	fallbackNotes []ContextNote
}

// answerPlans builds each member's answer ask. Web members get text files
// inlined; the others get every file attached, up to maxForwardBytes
// across the council, past which text is inlined. Binary files never
// reach web members. When uploads fail, text is inlined instead.
func answerPlans(c Council, files []fetched, nonce string) ([]plan, error) {
	var forwarded int64
	var plans []plan
	for _, s := range c.Members {
		p := plan{member: s.Name, out: client.Outgoing{To: s.Name}}
		var attach []fetched
		for _, f := range files {
			if !s.Web && forwarded+int64(len(f.data)) <= maxForwardBytes {
				forwarded += int64(len(f.data))
				attach = append(attach, f)
			}
		}
		var err error
		p.out.Body, p.notes, err = answerBody(c.Question, s.Name, files, attach, nonce)
		if err != nil {
			return nil, err
		}
		if len(attach) > 0 {
			for _, f := range attach {
				p.out.Files = append(p.out.Files, client.OutgoingFile{Name: f.name, MIME: f.mime, Data: f.data})
			}
			fb := client.Outgoing{To: s.Name}
			if fb.Body, p.fallbackNotes, err = answerBody(c.Question, s.Name, files, nil, nonce); err != nil {
				return nil, err
			}
			p.fallback = &fb
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// answerBody is member's answer prompt with the files in attach attached
// and the other text files inlined.
func answerBody(question, member string, files, attach []fetched, nonce string) (string, []ContextNote, error) {
	var inline []contextFile
	var attached, left []string
	for _, f := range files {
		switch {
		case slices.ContainsFunc(attach, func(a fetched) bool { return a.name == f.name }):
			attached = append(attached, f.name)
		case f.text:
			inline = append(inline, contextFile{Name: f.name, Text: string(f.data)})
		default:
			left = append(left, f.name)
		}
	}
	body, cut, err := answerPrompt(question, inline, attached, left, nonce)
	if err != nil {
		return "", nil, err
	}
	var notes []ContextNote
	for _, f := range inline {
		d := DeliveryInlined
		if slices.Contains(cut, f.Name) {
			d = DeliveryCut
		}
		notes = append(notes, ContextNote{File: f.Name, Member: member, Delivery: d})
	}
	for _, n := range attached {
		notes = append(notes, ContextNote{File: n, Member: member, Delivery: DeliveryAttached})
	}
	for _, n := range left {
		notes = append(notes, ContextNote{File: n, Member: member, Delivery: DeliveryLeftOut})
	}
	return body, notes, nil
}

// reviewFor builds reviewer's prompt. Every reviewer gets its own labels,
// drawn at random, and its own answer order, both kept here.
func (e *Engine) reviewFor(question string, inline []contextFile, reviewer string, answers []Answer, nonce string) (string, Review, []string, error) {
	labels := e.labels(len(answers))
	rev := Review{Reviewer: reviewer, Labels: map[string]string{}}
	var shown []labeledAnswer
	for _, i := range e.Rand.Perm(len(answers)) {
		rev.Labels[labels[i]] = answers[i].Member
		shown = append(shown, labeledAnswer{Label: labels[i], Text: answers[i].Copy})
	}
	body, cut, _, err := reviewPrompt(question, inline, shown, nonce)
	return body, rev, cut, err
}

// labels draws n distinct labels at random: single letters while they
// last, then pairs.
func (e *Engine) labels(n int) []string {
	var pool []string
	for c := 'A'; c <= 'Z'; c++ {
		pool = append(pool, string(c))
	}
	if n > len(pool) {
		for a := 'A'; a <= 'Z'; a++ {
			for b := 'A'; b <= 'Z'; b++ {
				pool = append(pool, string(a)+string(b))
			}
		}
	}
	e.Rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	return pool[:n]
}

// stage sends plans as children of parent in batches of maxBatch, each
// under its own group id, then waits up to limit for the replies. Asks
// still waiting to be claimed at the limit are cancelled. It returns each
// member's last known result and how context reached each member.
func (e *Engine) stage(ctx context.Context, parent string, plans []plan, limit time.Duration) (map[string]client.Result, []ContextNote, error) {
	res := map[string]client.Result{}
	var notes []ContextNote
	send := func(outs []client.Outgoing) []error {
		errs := make([]error, len(outs))
		for lo := 0; lo < len(outs); lo += maxBatch {
			batch := outs[lo:min(lo+maxBatch, len(outs))]
			g, berrs, err := e.Relay.SendEach(ctx, batch, envelope.KindAsk, parent)
			for i, o := range batch {
				switch {
				case err != nil:
					errs[lo+i] = err
					res[o.To] = failedSend(o.To, err)
				default:
					errs[lo+i] = berrs[i]
					res[o.To] = g.Results[i].Result
				}
			}
		}
		return errs
	}
	outs := make([]client.Outgoing, len(plans))
	for i, p := range plans {
		outs[i] = p.out
	}
	var retry []client.Outgoing
	for i, err := range send(outs) {
		p := plans[i]
		if p.fallback != nil && errors.Is(err, client.ErrUploadFailed) {
			retry = append(retry, *p.fallback)
			notes = append(notes, p.fallbackNotes...)
			continue
		}
		notes = append(notes, p.notes...)
	}
	if len(retry) > 0 {
		send(retry)
	}
	if err := e.gather(ctx, res, limit); err != nil {
		return nil, nil, err
	}
	return res, notes, nil
}

func failedSend(to string, err error) client.Result {
	return client.Result{Request: envelope.Request{To: to}, Status: envelope.StatusFailed,
		Reply: &envelope.Reply{From: to, Status: envelope.StatusFailed, Body: err.Error()}}
}

// gather polls the unfinished asks in res until each is done or limit
// passes, then cancels the ones not yet claimed.
func (e *Engine) gather(ctx context.Context, res map[string]client.Result, limit time.Duration) error {
	deadline := e.Now().Add(limit)
	var mu sync.Mutex
	poll := func(wait time.Duration) {
		open := map[string]string{} // member -> request id
		for m, r := range res {
			if r.Request.ID != "" && !r.Done() {
				open[m] = r.Request.ID
			}
		}
		var wg sync.WaitGroup
		for m, id := range open {
			wg.Go(func() {
				next, err := e.Relay.Get(ctx, id, wait)
				if err != nil {
					return // a later poll tries again
				}
				mu.Lock()
				res[m] = next
				mu.Unlock()
			})
		}
		wg.Wait()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !slices.ContainsFunc(mapValues(res), func(r client.Result) bool { return r.Request.ID != "" && !r.Done() }) {
			return nil
		}
		left := deadline.Sub(e.Now())
		if left <= 0 {
			break
		}
		if left >= time.Second {
			poll(min(left.Truncate(time.Second), client.MaxInlineWait))
			continue
		}
		poll(0)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.After(min(left, pollStep)):
		}
	}
	for m, r := range res {
		if r.Request.ID == "" || r.Done() {
			continue
		}
		switch r.Status {
		case envelope.StatusHeld, envelope.StatusQueued, envelope.StatusDelivered:
			if e.Relay.Cancel(ctx, r.Request.ID) == nil {
				continue // absent for the reason its status gives
			}
		}
		// Claimed, waiting on input, or it moved on before the cancel: a
		// last look may still find the reply.
		if next, err := e.Relay.Get(ctx, r.Request.ID, 0); err == nil && next.Done() {
			res[m] = next
		}
	}
	return ctx.Err()
}

func mapValues(m map[string]client.Result) []client.Result {
	out := make([]client.Result, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	return out
}

// absence is why a member whose last known result is r gave no answer in
// stage.
func absence(member, stage string, r client.Result) Absence {
	a := Absence{Member: member, Stage: stage, Reason: AbsentTimedOut}
	switch r.Status {
	case envelope.StatusFailed:
		a.Reason = AbsentFailed
	case envelope.StatusDeclined:
		a.Reason = AbsentDeclined
	case envelope.StatusHeld:
		a.Reason = AbsentHeld
	case envelope.StatusNeedsInput:
		a.Reason = AbsentNeedsInput
	}
	if r.Reply != nil && (a.Reason == AbsentFailed || a.Reason == AbsentDeclined) {
		a.Detail, _ = truncate(strings.TrimSpace(r.Reply.Body), 300)
	}
	return a
}

// elapsed is how long a member took from the ask to its reply.
func elapsed(r client.Result) time.Duration {
	if r.Reply == nil || r.Reply.CreatedAt.IsZero() || r.Request.CreatedAt.IsZero() {
		return 0
	}
	return r.Reply.CreatedAt.Sub(r.Request.CreatedAt)
}
