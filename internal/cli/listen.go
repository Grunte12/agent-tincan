package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func listenCmd() *cobra.Command {
	var execCmd string
	var once bool
	var sessionNotifications bool
	cmd := &cobra.Command{
		Use:   "listen --exec <command>",
		Short: "Stay connected and run a command whenever teammates' requests or replies are waiting",
		Long: `Hold a long-poll to the relay and run --exec when requests, or replies to
this agent's own requests, are waiting. Nothing is taken, so the agent picks
them up itself with check_inbox or "tincan inbox". The command gets
TINCAN_WAITING (requests plus unseen replies) in its environment and runs
through "sh -c". A new relay release also nudges the command, with its version
in TINCAN_UPGRADE_AVAILABLE (empty on other nudges), even if TINCAN_WAITING is zero.

Use this for agents whose runtime stays up and can be nudged by a command,
for example opening a Claude Code session in cmux. For agents that get a new
turn when a background command exits (Muse), use "tincan wait" instead.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if execCmd == "" {
				return errors.New("--exec is required")
			}
			r, _, err := connect()
			if err != nil {
				return err
			}
			return listenWithNotifications(cmd.Context(), r, execCmd, once, sessionNotifications)
		},
	}
	cmd.Flags().StringVar(&execCmd, "exec", "", "command to run when requests or replies are waiting")
	cmd.Flags().BoolVar(&once, "once", false, "exit after the first nudge")
	cmd.Flags().BoolVar(&sessionNotifications, "session-notifications", false, "wait for body-free native session request and reply notifications")
	return cmd
}

func listenWithNotifications(ctx context.Context, r *client.Relay, execCmd string, once, enabled bool) error {
	if !enabled {
		return listen(ctx, r, execCmd, once)
	}
	backoff := time.Second
	for {
		w, err := r.Notifications(ctx, client.DefaultPollHold)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case client.IsStatus(err, 403), client.IsProxyAuth(err):
			return err
		case err != nil:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jitter(backoff)):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if len(w.Notifications) == 0 {
			continue
		}
		if err := nudgeNotifications(ctx, r, execCmd, w, once); err != nil {
			if once || ctx.Err() != nil {
				return err
			}
			continue
		}
		if once {
			return nil
		}
	}
}

func nudgeNotifications(ctx context.Context, r *client.Relay, execCmd string, w client.Waiting, once bool) error {
	defer r.KeepPresence(ctx, client.Presence{Every: listenPresenceEvery, Cap: listenPresenceCap, Logf: listenLogf})()
	raw, err := json.Marshal(w.Notifications)
	if err != nil {
		return err
	}
	c := exec.CommandContext(ctx, "sh", "-c", execCmd)
	c.Env = append(os.Environ(), "TINCAN_WAITING="+strconv.Itoa(w.Total), "TINCAN_SESSION_NOTIFICATIONS="+string(raw))
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	err = c.Run()
	if err != nil {
		listenLogf("command failed: %v", err)
	}
	if once {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(listenCooldown):
		return err
	}
}

// listen's timing and log. Tests override them.
var (
	// listenPresenceEvery is how often listen refreshes the agent's relay
	// presence while its command runs and during the cooldown after it.
	listenPresenceEvery = client.DefaultPresenceInterval
	// listenPresenceCap bounds that refresh for one nudge, so an agent
	// whose command hangs falls offline again.
	listenPresenceCap = 30 * time.Minute
	// listenCooldown is the wait after a nudge before listen looks again.
	listenCooldown = 30 * time.Second
	// listenLog receives listen's own log lines.
	listenLog io.Writer = os.Stderr
)

func listen(ctx context.Context, r *client.Relay, execCmd string, once bool) error {
	backoff := time.Second
	// Failed pongs retry in the background while the loop keeps peeking; the
	// listener waits for them before it returns, --once included.
	var retries client.PongRetries
	defer retries.Wait()
	for {
		w, err := r.Peek(ctx, client.DefaultPollHold)
		n := w.Total - w.Pings
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case client.IsStatus(err, 403), client.IsProxyAuth(err):
			return err
		case err != nil:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jitter(backoff)):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		pingFailed := false
		for _, pending := range w.Pending {
			if pending.Kind != envelope.KindPing {
				continue
			}
			_, retry, err := client.AnswerPings(ctx, r, client.Inbox{Requests: []envelope.Request{{ID: pending.ID, Kind: envelope.KindPing}}}, "listen")
			if err != nil {
				pingFailed = true
			}
			retries.Go(ctx, retry)
		}
		if pingFailed && n <= 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(jitter(backoff)):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		nudged := false
		err = client.ReportUpgradeOn(client.UpgradeSurfaceListen, w.UpgradeAvailable, func(string) error {
			nudged = true
			return nudge(ctx, r, execCmd, n, once, w.UpgradeAvailable)
		})
		if !nudged && n > 0 {
			nudged = true
			err = nudge(ctx, r, execCmd, n, once, "")
		}
		if err != nil {
			if once || ctx.Err() != nil {
				return err
			}
			continue
		}
		if !nudged {
			continue
		}
		if once {
			return nil
		}
	}
}

// nudge runs the command for n waiting items and then, unless once, waits
// out the cooldown. The agent stays online throughout, up to
// listenPresenceCap: it is busy with what it was nudged about, not gone.
func nudge(ctx context.Context, r *client.Relay, execCmd string, n int, once bool, upgrade string) error {
	defer r.KeepPresence(ctx, client.Presence{
		Every: listenPresenceEvery,
		Cap:   listenPresenceCap,
		Logf:  listenLogf,
	})()
	c := exec.CommandContext(ctx, "sh", "-c", execCmd)
	c.Env = append(os.Environ(), "TINCAN_WAITING="+strconv.Itoa(n), "TINCAN_UPGRADE_AVAILABLE="+upgrade)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	err := c.Run()
	if err != nil {
		// A failed nudge is retried on the next loop; nothing was taken.
		listenLogf("command failed: %v", err)
	}
	if once {
		return err
	}
	// Give the agent time to pick them up before nudging again.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(listenCooldown):
		return err
	}
}

func listenLogf(format string, args ...any) {
	_, _ = fmt.Fprintf(listenLog, "tincan listen: "+format+"\n", args...)
}
