package cli

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
)

func searchCmd() *cobra.Command {
	var socket, relayURL string
	var limit int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "search <text>",
		Short: "Find past requests and replies in chains you took part in (admins see all)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 || limit > 50 {
				return errors.New("limit must be between 1 and 50")
			}
			r, err := adminRelay(socket, relayURL)
			if err != nil {
				return err
			}
			hits, err := r.Search(cmd.Context(), strings.Join(args, " "), limit)
			if err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(cmd.OutOrStdout(), hits)
			}
			for _, hit := range hits {
				cmd.Printf("%s  %s  %s -> %s  %s  %s\n", hit.TraceID, hit.RequestID, hit.From, hit.To, hit.Status, oneLine(hit.Snippet))
				if len(hit.AttachmentNames) > 0 {
					cmd.Printf("  Attachments: %s\n", strings.Join(hit.AttachmentNames, ", "))
				}
			}
			if len(hits) == 0 {
				cmd.Println("No matching requests or replies.")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum matches (1-50)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print results as JSON")
	cmd.Flags().StringVar(&socket, "socket", "", "relay admin socket (when running on the relay host)")
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL (default: saved config)")
	return cmd
}
