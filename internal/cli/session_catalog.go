package cli

import (
	"errors"

	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"github.com/spf13/cobra"
)

func sessionDiscoverCmd() *cobra.Command {
	var relayURL, host, project string
	cmd := &cobra.Command{
		Use:   "session-discover",
		Short: "Register this native conversation for project routing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if client.RuntimeSessionID() == "" {
				return errors.New("native runtime session ID required")
			}
			r, _, err := relayFor(relayURL)
			if err != nil {
				return err
			}
			binding, err := r.DiscoverSession(cmd.Context(), host, project)
			if err != nil {
				return err
			}
			return printJSON(cmd, binding)
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config or TINCAN_RELAY)")
	cmd.Flags().StringVar(&host, "host", "", "native host: codex, cursor, antigravity, muse, claude-code, opencode, or hermes")
	cmd.Flags().StringVar(&project, "project", "", "absolute native project path")
	_ = cmd.MarkFlagRequired("host")
	_ = cmd.MarkFlagRequired("project")
	return cmd
}

func projectSessionCmd() *cobra.Command {
	var relayURL, host, project string
	cmd := &cobra.Command{
		Use:   "project-session <agent>",
		Short: "Resolve the immutable owner of an existing native project conversation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if client.RuntimeSessionID() == "" {
				return errors.New("native runtime session ID required")
			}
			r, _, err := relayFor(relayURL)
			if err != nil {
				return err
			}
			binding, err := r.ResolveProjectSession(cmd.Context(), args[0], host, project)
			if err != nil {
				return err
			}
			return printJSON(cmd, binding)
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config or TINCAN_RELAY)")
	cmd.Flags().StringVar(&host, "host", "", "native host: codex, cursor, antigravity, muse, claude-code, opencode, or hermes")
	cmd.Flags().StringVar(&project, "project", "", "absolute native project path")
	_ = cmd.MarkFlagRequired("host")
	_ = cmd.MarkFlagRequired("project")
	return cmd
}

func sessionRetireCmd() *cobra.Command {
	var relayURL, host, sessionID string
	cmd := &cobra.Command{
		Use:   "session-retire",
		Short: "Retire this agent's deleted native conversation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if client.RuntimeSessionID() == "" {
				return errors.New("native originating session ID required")
			}
			r, _, err := relayFor(relayURL)
			if err != nil {
				return err
			}
			changed, err := r.RetireSession(cmd.Context(), host, sessionID)
			if err != nil {
				return err
			}
			return printJSON(cmd, map[string]any{"session_id": sessionID, "changed": changed})
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config or TINCAN_RELAY)")
	cmd.Flags().StringVar(&host, "host", "", "native host: codex, cursor, antigravity, muse, claude-code, opencode, or hermes")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "deleted native session ID owned by this agent")
	_ = cmd.MarkFlagRequired("host")
	_ = cmd.MarkFlagRequired("session-id")
	return cmd
}

func sessionPreserveCmd() *cobra.Command {
	var socket, relayURL, agent, host, sessionID, project string
	cmd := &cobra.Command{
		Use:   "session-preserve",
		Short: "Preserve a verified existing native session owner (admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			binding := envelope.SessionBinding{Agent: agent, Host: host, Project: project, SessionID: sessionID}
			var result struct {
				Binding envelope.SessionBinding `json:"binding"`
				Changed bool                    `json:"changed"`
			}
			if err := r.Raw(cmd.Context(), "POST", "/v1/admin/session-owners/preserve", binding, &result); err != nil {
				return err
			}
			return printJSON(cmd, result)
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: local admin socket)")
	cmd.Flags().StringVar(&agent, "agent", "", "joined recipient agent name")
	cmd.Flags().StringVar(&host, "host", "", "native host: codex, cursor, antigravity, muse, claude-code, opencode, or hermes")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "verified existing native session UUID")
	cmd.Flags().StringVar(&project, "project", "", "absolute project path, when verified")
	for _, flag := range []string{"agent", "host", "session-id"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}
