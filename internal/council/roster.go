package council

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/mvanhorn/agent-tincan/internal/onboard"
	"github.com/mvanhorn/agent-tincan/internal/policy"
)

// MinMembers is the smallest council: fewer members cannot rank each
// other meaningfully once each one's vote on its own answer is dropped.
const MinMembers = 3

// Exclusion reasons, one label per reason.
const (
	ReasonInChain     = "excluded (in chain)"
	ReasonLiveSession = "excluded (live session)"
	ReasonService     = "excluded (service)"
	ReasonNotWakeable = "excluded (not wakeable)"
	ReasonByOwner     = "excluded (by owner)"
	ReasonByRequest   = "excluded (by request)"
)

// Exclusion is a roster agent that does not sit on a council, and why.
type Exclusion struct {
	Name   string
	Reason string
}

// Eligibility is who sits on one council and who chairs it.
type Eligibility struct {
	// Members sit on the council, in roster order.
	Members []string
	// Chairmen is the chairman failover order.
	Chairmen []string
	// Excluded is every other roster agent with its reason, in roster
	// order. An agent a form's members list leaves out is not listed.
	Excluded []Exclusion
	// Decline, when set, is why the council must be declined before any
	// member is asked.
	Decline string
}

// Resolve decides eligibility for a convene request from the roster, the
// convening request's From, Chain, and Hop, council.json, and the form's
// overrides. Kind decides eligibility, not the online flag: web agents and
// woken-on-demand model agents sit by default; services, live sessions,
// and agents with no way to wake them do not, unless the owner lists them
// in council.json. The convener and everyone in its chain never sit or
// chair, so a council cannot widen who reaches whom.
func Resolve(roster []onboard.Member, conv envelope.Request, cfg Config, req Request) Eligibility {
	var e Eligibility
	// A convening request at the relay's hop limit leaves no hop for the
	// asks Council would send to its members.
	if conv.Hop >= policy.DefaultHopLimit {
		e.Decline = fmt.Sprintf("the request chain is too deep: it is at hop %d of %d, so Council cannot ask anyone", conv.Hop, policy.DefaultHopLimit)
		return e
	}
	inChain := func(name string) bool { return name == conv.From || slices.Contains(conv.Chain, name) }
	listed := func(name string) bool {
		return slices.Contains(cfg.Members, name) || slices.Contains(cfg.Chairmen, name)
	}
	profiles := map[string]onboard.Profile{}
	for _, m := range roster {
		profiles[m.Name] = onboard.ProfileOf(m)
	}
	// mayName is "" for an agent that may sit or chair when named, else
	// the reason it may not.
	mayName := func(name string) string {
		if listed(name) {
			return ""
		}
		return kindReason(profiles[name])
	}
	for _, name := range append(slices.Clone(req.Members), req.Chairman) {
		if name == "" {
			continue
		}
		if _, ok := profiles[name]; !ok {
			e.Decline = fmt.Sprintf("%s is not on the roster", name)
			return e
		}
		if r := mayName(name); r != "" && !inChain(name) {
			e.Decline = fmt.Sprintf("%s cannot sit on a council: %s. The owner can list it in council.json", name, r)
			return e
		}
	}

	for _, m := range roster {
		name := m.Name
		reason := ""
		switch {
		case req.Members != nil && !slices.Contains(req.Members, name):
			continue
		case inChain(name):
			reason = ReasonInChain
		case req.Members != nil:
		case slices.Contains(req.Exclude, name):
			reason = ReasonByRequest
		case slices.Contains(cfg.Exclude, name):
			reason = ReasonByOwner
		case slices.Contains(cfg.Members, name):
		default:
			reason = kindReason(profiles[name])
		}
		if reason != "" {
			e.Excluded = append(e.Excluded, Exclusion{Name: name, Reason: reason})
			continue
		}
		e.Members = append(e.Members, name)
	}

	var candidates []string
	if req.Chairman != "" {
		candidates = append(candidates, req.Chairman)
	}
	for _, c := range cfg.Chairmen {
		if _, ok := profiles[c]; ok {
			candidates = append(candidates, c)
			continue
		}
		// Not an agent name: every agent of that kind, in roster order.
		for _, m := range roster {
			if profiles[m.Name].Kind == c && mayName(m.Name) == "" {
				candidates = append(candidates, m.Name)
			}
		}
	}
	for _, c := range candidates {
		if !inChain(c) && !slices.Contains(e.Chairmen, c) {
			e.Chairmen = append(e.Chairmen, c)
		}
	}

	if len(e.Members) < MinMembers {
		e.Decline = fmt.Sprintf("only %d eligible member(s) (%s), and a council needs at least %d", len(e.Members), orNone(e.Members), MinMembers)
	}
	return e
}

// kindReason is why an agent of profile p does not sit by default, or ""
// when it does.
func kindReason(p onboard.Profile) string {
	switch {
	case p.Web:
		return ""
	case p.Service || p.Kind == onboard.KindScheduled:
		return ReasonService
	case p.Kind == onboard.KindClaudeCode || p.Wake == "channel":
		return ReasonLiveSession
	case p.Wake == "command" || p.Wake == "webhook" || p.Wake == "email" || p.ExpectOnline:
		return ""
	}
	return ReasonNotWakeable
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
