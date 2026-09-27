// Package policy fills in a request's chain and stops runaway loops between
// trusted agents. It never restricts who may ask whom: joined agents trust
// each other. It only enforces the hop limit, rejects cycles, and rate-limits
// each sender as a backstop against an agent stuck starting new chains.
package policy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/relay"
	"github.com/mvanhorn/agent-tincan/internal/store"
)

var (
	ErrCycle         = errors.New("request would loop back to an agent already in this chain")
	ErrHopLimit      = errors.New("request chain is too long")
	ErrBadParent     = errors.New("parent request is not one this agent is handling")
	ErrUrgentLimited = errors.New("urgent limit reached; send without --urgent")
	ErrRateLimited   = errors.New("too many requests from this agent; slow down")
)

// Config tunes the policy.
type Config struct {
	UrgentPerHour int // max urgent requests per sender per hour; default 5
	HopLimit      int // longest allowed chain; default 4
	PerMinute     int // max new requests per sender per minute; default 30
	Now           func() time.Time
}

// Policy implements relay.Preparer.
type Policy struct {
	st  *store.Store
	cfg Config

	mu     sync.Mutex
	sent   map[string][]time.Time
	urgent map[string][]time.Time
}

// New builds a Policy over the relay store.
func New(st *store.Store, cfg Config) *Policy {
	if cfg.HopLimit == 0 {
		cfg.HopLimit = 4
	}
	if cfg.PerMinute == 0 {
		cfg.PerMinute = 30
	}
	if cfg.UrgentPerHour == 0 {
		cfg.UrgentPerHour = 5
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Policy{st: st, cfg: cfg, sent: map[string][]time.Time{}, urgent: map[string][]time.Time{}}
}

// Prepare sets TraceID, Hop, Chain, and ParentID, then applies the loop and
// rate checks.
func (p *Policy) Prepare(ctx context.Context, req *envelope.Request) error {
	parent, ok, err := p.parent(ctx, req)
	if err != nil {
		return err
	}
	if ok {
		req.ParentID, req.TraceID, req.Hop = parent.ID, parent.TraceID, parent.Hop+1
		req.Chain = append(slices.Clone(parent.Chain), req.From)
	} else {
		req.ParentID, req.TraceID, req.Hop, req.Chain = "", "", 1, []string{req.From}
	}
	if slices.Contains(req.Chain, req.To) {
		return reject(http.StatusConflict, fmt.Errorf("%w: %s is already in %v", ErrCycle, req.To, req.Chain))
	}
	if req.Hop > p.cfg.HopLimit {
		return reject(http.StatusConflict, fmt.Errorf("%w: hop %d exceeds %d", ErrHopLimit, req.Hop, p.cfg.HopLimit))
	}
	return p.rate(req.From, req.Urgent)
}

// parent resolves the request's parent: the one it names, or else the
// request the sender currently has claimed. The model cannot opt out of a
// chain by leaving the parent blank.
func (p *Policy) parent(ctx context.Context, req *envelope.Request) (envelope.Request, bool, error) {
	if req.ParentID == "" {
		return p.st.OpenClaim(ctx, req.From)
	}
	parent, status, err := p.st.Request(ctx, req.ParentID)
	if errors.Is(err, store.ErrNotFound) {
		return envelope.Request{}, false, reject(http.StatusBadRequest, ErrBadParent)
	}
	if err != nil {
		return envelope.Request{}, false, err
	}
	if parent.To != req.From || (status != envelope.StatusClaimed && status != envelope.StatusDelivered) {
		return envelope.Request{}, false, reject(http.StatusBadRequest, ErrBadParent)
	}
	return parent, true, nil
}

// Refund returns the urgent slot req took in Prepare, for a send the relay
// then failed to queue (a bad attachment, say), so a failed send never
// spends the sender's urgent allowance.
func (p *Policy) Refund(req envelope.Request) {
	if !req.Urgent {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := len(p.urgent[req.From]); n > 0 {
		p.urgent[req.From] = p.urgent[req.From][:n-1]
	}
}

func (p *Policy) rate(sender string, urgent bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	cutoff := now.Add(-time.Minute)
	recent := slices.DeleteFunc(p.sent[sender], func(t time.Time) bool { return !t.After(cutoff) })
	if len(recent) >= p.cfg.PerMinute {
		p.sent[sender] = recent
		return reject(http.StatusTooManyRequests, ErrRateLimited)
	}
	if urgent {
		cutoff := now.Add(-time.Hour)
		recentUrgent := slices.DeleteFunc(p.urgent[sender], func(t time.Time) bool { return !t.After(cutoff) })
		p.urgent[sender] = recentUrgent
		if len(recentUrgent) >= p.cfg.UrgentPerHour {
			return reject(http.StatusTooManyRequests, ErrUrgentLimited)
		}
		p.urgent[sender] = append(recentUrgent, now)
	}
	p.sent[sender] = append(recent, now)
	return nil
}

func reject(code int, err error) error { return &relay.StatusError{Code: code, Err: err} }
