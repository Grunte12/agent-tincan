package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	textwidth "golang.org/x/text/width"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

type topFrame struct {
	roster client.Roster
	held   []envelope.Request
	chains []envelope.Result
	admin  bool
	note   string
	at     time.Time
}

func topCmd() *cobra.Command {
	var socket, relayURL string
	var once bool
	var interval time.Duration
	cmd := &cobra.Command{Use: "top", Short: "Watch the mesh live (read-only)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < time.Second {
				return fmt.Errorf("interval must be at least 1s")
			}
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			out, ok := cmd.OutOrStdout().(*os.File)
			live := !once && ok && term.IsTerminal(int(out.Fd())) && term.IsTerminal(int(os.Stdin.Fd()))
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if live {
				state, err := term.MakeRaw(int(os.Stdin.Fd()))
				if err != nil {
					return err
				}
				defer func() { _ = term.Restore(int(os.Stdin.Fd()), state) }()
				done := make(chan struct{})
				go func() {
					defer close(done)
					var key [1]byte
					for {
						if ctx.Err() != nil {
							return
						}
						fds := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}}
						n, err := unix.Poll(fds, 100)
						if err == unix.EINTR {
							continue
						}
						if err != nil {
							cancel()
							return
						}
						if n == 0 {
							continue
						}
						n, err = unix.Read(int(os.Stdin.Fd()), key[:])
						if err == unix.EAGAIN || err == unix.EINTR {
							continue
						}
						if err != nil || n == 0 {
							cancel()
							return
						}
						if key[0] == 'q' || key[0] == 3 {
							cancel()
							return
						}
					}
				}()
				defer func() { cancel(); <-done }()
			}
			for {
				f, err := fetchFrame(ctx, r)
				if ctx.Err() != nil {
					return nil
				}
				if err != nil {
					return err
				}
				width, height := 120, 24
				if live {
					if w, h, err := term.GetSize(int(out.Fd())); err == nil && w > 0 && h > 0 {
						width, height = w, h
					}
				}
				frame := renderFrame(f, width)
				if live {
					frame = "\x1b[H\x1b[2J" + strings.ReplaceAll(topLiveFrame(frame, width, height), "\n", "\r\n")
				}
				if _, err := fmt.Fprint(cmd.OutOrStdout(), frame); err != nil {
					return err
				}
				if !live {
					return nil
				}
				timer := time.NewTimer(interval)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
			}
		}}
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	cmd.Flags().BoolVar(&once, "once", false, "print one plain snapshot and exit")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "refresh interval (minimum 1s)")
	return cmd
}

func fetchFrame(ctx context.Context, r *client.Relay) (topFrame, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	f := topFrame{at: time.Now()}
	var err error
	f.roster, err = r.Roster(ctx)
	if err != nil {
		return f, err
	}
	if err = r.Raw(ctx, "GET", "/v1/admin/held", nil, &f.held); err != nil {
		if e, ok := errors.AsType[*client.APIError](err); ok && (e.Code == 403 || e.Code == 404) {
			f.note = "held and recent chains need an admin device"
			if e.Code == 404 {
				f.note = "held and recent chains unavailable on this relay"
			}
			return f, nil
		}
		return f, err
	}
	f.admin = true
	var out struct {
		Traces []envelope.Result `json:"traces"`
	}
	if err = r.Raw(ctx, "GET", "/v1/trace?limit=8&exclude_pings=true", nil, &out); err != nil {
		if e, ok := errors.AsType[*client.APIError](err); ok && (e.Code == 403 || e.Code == 404) {
			f.note = "recent chains unavailable on this relay"
			return f, nil
		}
		return f, err
	}
	f.chains = filterPingTraces(out.Traces, false)
	return f, nil
}

func attention(a client.AgentInfo, relayVersion string, now time.Time) (score int, flags []string) {
	overdue := a.OverdueNote(now) != ""
	if !a.Online && a.Queued > 0 && (a.Wake == "" || a.Wake == "none" || overdue) {
		score += 8
		flags = append(flags, "QUEUED-OFFLINE")
	}
	if a.Queued > 0 && !a.OldestQueued.IsZero() && now.Sub(a.OldestQueued) > time.Hour {
		score += 4
		flags = append(flags, "STALE")
	}
	if overdue {
		score += 2
		flags = append(flags, "OVERDUE")
	}
	if client.Ahead(relayVersion, a.Version) {
		score++
		flags = append(flags, "OLD BUILD")
	}
	if a.Claimed > 0 {
		flags = append(flags, "CLAIMED")
	}
	return
}

func renderFrame(f topFrame, width int) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(topText(s, max(width, 1))); b.WriteByte('\n') }
	online, queued := 0, 0
	for _, a := range f.roster.Agents {
		if a.Online {
			online++
		}
		queued += a.Queued
	}
	held := "?"
	if f.admin {
		held = fmt.Sprint(len(f.held))
	}
	line(fmt.Sprintf("tincan top | relay %s | online %d/%d | queued %d | held %s | %s", f.roster.RelayVersion, online, len(f.roster.Agents), queued, held, f.at.Format("15:04:05")))
	line("AGENT            STATE   WAKE         QUEUED OLDEST CLAIMS VERSION")
	agents := slices.Clone(f.roster.Agents)
	slices.SortStableFunc(agents, func(a, b client.AgentInfo) int {
		sa, _ := attention(a, f.roster.RelayVersion, f.at)
		sb, _ := attention(b, f.roster.RelayVersion, f.at)
		if sa != sb {
			return sb - sa
		}
		return strings.Compare(a.Name, b.Name)
	})
	for _, a := range agents {
		_, flags := attention(a, f.roster.RelayVersion, f.at)
		age := "-"
		if a.Queued > 0 && !a.OldestQueued.IsZero() {
			age = max(f.at.Sub(a.OldestQueued), 0).Round(time.Second).String()
		}
		line(fmt.Sprintf("%-16s %-7s %-12s %6d %6s %6d %s", topText(a.Name, 16), a.State(), topText(a.Wake, 12), a.Queued, age, a.Claimed, a.Version))
		if len(flags) > 0 {
			line("  " + strings.Join(flags, " | "))
		}
	}
	if f.admin {
		line(fmt.Sprintf("Held for approval (%d):", len(f.held)))
		for _, r := range f.held {
			line(fmt.Sprintf("  %s %s -> %s %s", r.ID, r.From, r.To, topText(r.Body, 60)))
		}
		line("Recent chains:")
		for _, r := range f.chains {
			line(fmt.Sprintf("  %s %s -> %s [%s] %s", r.Request.CreatedAt.Format("15:04:05"), r.Request.From, r.Request.To, r.Status, topText(r.Request.Body, 60)))
		}
	}
	if f.note != "" {
		line(f.note)
	}
	return b.String()
}

// Relay text is data, including when it contains terminal control sequences.
func topText(s string, width int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	cells := 0
	for _, r := range s {
		cells += topRuneWidth(r)
	}
	if cells <= width {
		return s
	}
	limit := max(width, 0)
	suffix := ""
	if width > 3 {
		limit -= 3
		suffix = "..."
	}
	cells = 0
	for i, r := range s {
		cells += topRuneWidth(r)
		if cells > limit {
			return s[:i] + suffix
		}
	}
	return s
}

// Bound only interactive frames; snapshots retain every rendered line.
func topLiveFrame(frame string, width, height int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
	if len(lines) > height {
		omitted := len(lines) - height + 1
		lines = lines[:height]
		lines[height-1] = topText(fmt.Sprintf("... %d more lines (run tincan top --once for all)", omitted), width)
	}
	return strings.Join(lines, "\n")
}

func topRuneWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	kind := textwidth.LookupRune(r).Kind()
	if kind == textwidth.EastAsianWide || kind == textwidth.EastAsianFullwidth ||
		(r >= 0x1f000 && r <= 0x1faff) || (r >= 0x2600 && r <= 0x27bf) {
		return 2
	}
	return 1
}
