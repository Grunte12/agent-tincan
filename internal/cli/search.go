package cli

import (
	"errors"
	"fmt"
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
				if hit.ReplySnippet != "" {
					cmd.Printf("  Reply: %s\n", oneLine(hit.ReplySnippet))
				}
				if hit.QuestionSnippet != "" {
					cmd.Printf("  Question: %s\n", oneLine(hit.QuestionSnippet))
				}
				if len(hit.AttachmentNames) > 0 {
					cmd.Printf("  Attachments: %s\n", searchAttachmentNames(hit.AttachmentNames))
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

// searchAttachmentNames bounds human-readable output; JSON retains all names.
func searchAttachmentNames(names []string) string {
	shown := make([]string, 0, 6)
	for _, name := range names[:min(len(names), 5)] {
		chars := []rune(strings.Join(strings.Fields(name), " "))
		if len(chars) > 80 {
			chars = append(chars[:79], '…')
		}
		shown = append(shown, string(chars))
	}
	if len(names) > 5 {
		shown = append(shown, fmt.Sprintf("and %d more", len(names)-5))
	}
	return strings.Join(shown, ", ")
}
