package onboard

import (
	"strings"
	"testing"
)

// KTD6: the operator explains wake=schedule when the team uses it: nothing
// to wake, replies come within the interval, and a missed check means the
// agent's own cron stopped.
func TestOperatorTroubleshootingSchedule(t *testing.T) {
	out, err := execute("troubleshooting", operatorData{Owner: "Matt", OwnerPoss: "Matt's", RelayURL: "http://relay", WakeMethods: []string{"schedule"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Schedule wakes:", "nothing to wake", "within its interval", "overdue", "its own cron", "stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("troubleshooting lacks %q:\n%s", want, out)
		}
	}
	out, err = execute("troubleshooting", operatorData{Owner: "Matt", OwnerPoss: "Matt's", RelayURL: "http://relay", WakeMethods: []string{"webhook"}})
	if err != nil || strings.Contains(out, "Schedule wakes:") {
		t.Fatalf("webhook-only troubleshooting = %q, %v", out, err)
	}
}

// R4: the operator's health check reads the overdue marker tincan agents
// shows for a schedule agent.
func TestOperatorHealthNamesOverdue(t *testing.T) {
	k, err := Build(Options{RelayURL: "http://relay", Owner: "Matt"})
	if err != nil {
		t.Fatal(err)
	}
	var presence string
	for line := range strings.SplitSeq(k.Operator, "\n") {
		if strings.HasPrefix(line, "Agent presence:") {
			presence = line
		}
	}
	if !strings.Contains(presence, "wake=schedule") || !strings.Contains(presence, "overdue: last check") {
		t.Fatalf("presence line = %q", presence)
	}
}
