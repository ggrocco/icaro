package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"icaro/internal/apiclient"
	"icaro/internal/assist"
	"icaro/internal/assist/stream"
	"icaro/internal/store"
)

func init() { register(newAssistCmd) }

func newAssistCmd(opts *rootOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assist",
		Short: "Talk to the workflow assistant (the chat beside the web UI's editor)",
		Long: "The workflow assistant runs your profile's own agent CLI (claude or codex) under its login, so a subscription works " +
			"without an API key. It reads the schema, integrations and runs, and proposes drafts; you apply them. Runs it asks for " +
			"wait for your approval. See docs/assistant.md.",
	}
	var asJSON bool
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")
	withClient := func(run func(ctx context.Context, c *apiclient.Client, cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			return run(cmd.Context(), c, cmd, args)
		}
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List chats",
		Args:  cobra.NoArgs,
		RunE: withClient(func(ctx context.Context, c *apiclient.Client, cmd *cobra.Command, _ []string) error {
			chats, err := c.ListChats(ctx)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), chats)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTITLE\tWORKFLOW\tAGENT\tCONTAINER\tUPDATED")
			for _, ch := range chats {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", ch.ID, dash(ch.Title), dash(ch.Workflow), ch.Agent, dash(ch.SessionStatus), ch.UpdatedAt.Local().Format(time.DateTime))
			}
			return tw.Flush()
		}),
	}

	var newReq assist.CreateRequest
	var yamlFile string
	newChat := &cobra.Command{
		Use:   "new",
		Short: "Start a chat (optionally about a saved workflow or a draft file)",
		Args:  cobra.NoArgs,
		RunE: withClient(func(ctx context.Context, c *apiclient.Client, cmd *cobra.Command, _ []string) error {
			if yamlFile != "" {
				b, err := readInputArg(yamlFile)
				if err != nil {
					return err
				}
				newReq.YAML = string(b)
			}
			d, err := c.CreateChat(ctx, newReq)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), d)
			}
			fmt.Fprintln(cmd.OutOrStdout(), d.ID)
			return nil
		}),
	}
	newChat.Flags().StringVar(&newReq.Workflow, "workflow", "", "start from this saved workflow")
	newChat.Flags().StringVarP(&yamlFile, "file", "f", "", "start from this draft (a path, or - for stdin)")
	newChat.Flags().StringVar(&newReq.Title, "title", "", "the chat's title (default: the first message)")
	newChat.Flags().StringVar(&newReq.ProjectID, "project-id", "", "scope the assistant's memory reads to this project")

	show := &cobra.Command{
		Use:   "show CHAT",
		Short: "Show a chat's messages, draft and pending approvals",
		Args:  cobra.ExactArgs(1),
		RunE: withClient(func(ctx context.Context, c *apiclient.Client, cmd *cobra.Command, args []string) error {
			d, err := c.GetChat(ctx, args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), d)
			}
			printChat(cmd.OutOrStdout(), d)
			return nil
		}),
	}

	var noWait bool
	send := &cobra.Command{
		Use:   "send CHAT MESSAGE...",
		Short: "Send a message and print the reply as it streams",
		Args:  cobra.MinimumNArgs(2),
		RunE: withClient(func(ctx context.Context, c *apiclient.Client, cmd *cobra.Command, args []string) error {
			msgs, err := c.SendChat(ctx, args[0], strings.Join(args[1:], " "))
			if err != nil {
				return err
			}
			if noWait || len(msgs) < 2 {
				if asJSON {
					return printJSON(cmd.OutOrStdout(), msgs)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "queued", msgs[len(msgs)-1].ID)
				return nil
			}
			reply := msgs[len(msgs)-1]
			final, err := followReply(ctx, c, args[0], reply, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), final)
			}
			return replyError(final)
		}),
	}
	send.Flags().BoolVar(&noWait, "no-wait", false, "queue the message and return")

	cancel := &cobra.Command{
		Use:   "cancel CHAT",
		Short: "Stop the chat's running turn and drop its queued ones",
		Args:  cobra.ExactArgs(1),
		RunE: withClient(func(ctx context.Context, c *apiclient.Client, _ *cobra.Command, args []string) error {
			return c.CancelChat(ctx, args[0])
		}),
	}

	decide := func(approve bool) *cobra.Command {
		verb, short := "decline", "Decline a run the assistant asked for"
		if approve {
			verb, short = "approve", "Approve a run the assistant asked for"
		}
		return &cobra.Command{
			Use:   verb + " CHAT APPROVAL",
			Short: short,
			Args:  cobra.ExactArgs(2),
			RunE: withClient(func(ctx context.Context, c *apiclient.Client, cmd *cobra.Command, args []string) error {
				a, err := c.DecideChat(ctx, args[0], args[1], approve)
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(cmd.OutOrStdout(), a)
				}
				fmt.Fprintln(cmd.OutOrStdout(), a.Decision)
				return nil
			}),
		}
	}

	var yes bool
	rm := &cobra.Command{
		Use:   "rm CHAT",
		Short: "Delete a chat, its container and its log",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				ok, err := confirm(cmd.InOrStdin(), cmd.ErrOrStderr(), fmt.Sprintf("Delete chat %s, its container and its log?", args[0]))
				if err != nil || !ok {
					return err
				}
			}
			c, err := opts.client(cmd.Context())
			if err != nil {
				return err
			}
			return c.DeleteChat(cmd.Context(), args[0])
		},
	}
	rm.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")

	cmd.AddCommand(list, newChat, show, send, cancel, decide(true), decide(false), rm)
	return cmd
}

func printChat(w io.Writer, d *assist.ChatDetail) {
	fmt.Fprintf(w, "%s  %s\n", d.ID, dash(d.Title))
	fmt.Fprintf(w, "agent %s (profile %s), workflow %s, container %s\n\n", d.Agent, d.Profile, dash(d.Workflow), dash(d.SessionStatus))
	for _, m := range d.Messages {
		who := "you"
		if m.Role == store.RoleAssistant {
			who = "assistant"
		}
		fmt.Fprintf(w, "[%s] %s\n", who, strings.TrimSpace(m.Text))
		if m.Status != store.TurnDone {
			fmt.Fprintf(w, "  (%s) %s\n", m.Status, m.Error)
		}
	}
	if d.Draft != nil {
		var issues []json.RawMessage
		_ = json.Unmarshal(d.Draft.Issues, &issues)
		fmt.Fprintf(w, "\ndraft v%d by %s, %d issue(s)\n", d.Draft.Version, d.Draft.Source, len(issues))
	}
	for _, a := range d.Approvals {
		fmt.Fprintf(w, "\nwaiting for you: %s %s\n  icaro assist approve %s %s\n", a.Tool, a.Request, d.ID, a.ID)
	}
}

// chatLogEntry is the part of a chat log entry the CLI reads.
type chatLogEntry struct {
	Type      string               `json:"type"`
	MessageID string               `json:"message_id"`
	Status    string               `json:"status"`
	Note      string               `json:"note"`
	Message   *assist.MessageInfo  `json:"message"`
	Event     *stream.Event        `json:"event"`
	Approval  *assist.ApprovalInfo `json:"approval"`
}

// followReply prints a reply's text and tool calls as they stream, and
// returns the settled message.
func followReply(ctx context.Context, c *apiclient.Client, chatID string, reply assist.MessageInfo, out io.Writer) (*assist.MessageInfo, error) {
	offset := reply.LogOffset
	for {
		var final *assist.MessageInfo
		err := c.ChatEvents(ctx, chatID, offset, func(e apiclient.ChatEntry) bool {
			if e.Resync {
				return false
			}
			offset = e.Offset
			var en chatLogEntry
			if json.Unmarshal(e.Data, &en) != nil {
				return true
			}
			switch {
			case en.Type == assist.EntryApproval && en.Approval != nil && en.Approval.Decision == "":
				fmt.Fprintf(out, "\n[waiting for you] %s %s\n  icaro assist approve %s %s   (or decline)\n", en.Approval.Tool, en.Approval.Request, chatID, en.Approval.ID)
			case en.MessageID != reply.ID:
			case en.Type == assist.EntryEvent && en.Event != nil:
				switch en.Event.Kind {
				case stream.KindText:
					fmt.Fprint(out, en.Event.Text)
				case stream.KindToolCall:
					fmt.Fprintf(out, "\n→ %s\n", en.Event.Tool)
				}
			case en.Type == assist.EntryStatus && en.Note != "" && en.Status == store.TurnQueued:
				fmt.Fprintln(out, en.Note)
			case en.Type == assist.EntryStatus && en.Status != store.TurnQueued && en.Status != store.TurnRunning:
				if en.Message != nil {
					final = en.Message
				} else {
					final = &assist.MessageInfo{ID: reply.ID, Status: en.Status, Error: en.Note}
				}
				return false
			}
			return true
		})
		if final != nil {
			fmt.Fprintln(out)
			return final, nil
		}
		if err != nil {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// replyError turns a reply that did not finish into the command's error.
func replyError(m *assist.MessageInfo) error {
	switch m.Status {
	case store.TurnDone:
		return nil
	case store.TurnAuthRequired:
		return fmt.Errorf("the assistant's profile is not signed in (%s); run icaro login", m.Error)
	case store.TurnRateLimited:
		msg := "the provider's usage limit was reached"
		if m.ResetAt != nil {
			msg += "; it resets at " + m.ResetAt.Local().Format(time.DateTime)
		}
		return errors.New(msg)
	}
	return fmt.Errorf("the reply %s: %s", m.Status, m.Error)
}
