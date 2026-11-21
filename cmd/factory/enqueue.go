package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/autobutler-org/factory/internal/apiclient"
	"github.com/autobutler-org/factory/internal/work"
)

func workEnqueueCommand() command {
	return command{
		name:    "work enqueue",
		summary: "Enqueue work by value, or from a JSON file.",
		about: `Enqueue one work item by value: --title and the other flags, or --file with
the JSON body POST /api/v1/work takes (docs/api.md). Enqueueing an item that
exists, by --source and --external-id or by --idempotency-key, returns it and
creates nothing. Enqueueing by tracker URL waits on the input adapters (#7, #18).`,
		env: clientEnv,
		setup: func(fs *flag.FlagSet) runner {
			cf := addClientFlags(fs)
			file := fs.String("file", "", "read the item from this JSON `file`; - reads stdin. No other item flag may be given with it")
			title := fs.String("title", "", "the item's `title` (required without --file)")
			body := fs.String("body", "", "the item's description, the brief an agent works from")
			repos := fs.String("repos", "", "comma-separated `repositories` (owner/name) the work changes")
			source := fs.String("source", "", "the tracker the item comes from, such as github")
			externalID := fs.String("external-id", "", "the item's `ID` in its source, such as autobutler-org/quark#123")
			externalURL := fs.String("external-url", "", "the item's `URL` in its source")
			state := fs.String("state", "", "design or ready (the server's default is ready)")
			priority := fs.Int("priority", 0, "higher is handed out first")
			blockedBy := fs.String("blocked-by", "", "comma-separated `items` (IDs or source:external_id) that must finish first")
			requires := fs.String("requires", "", "comma-separated `capabilities` a worker needs, such as claude")
			key := fs.String("idempotency-key", "", "a `key` for work with no source: a repeat with the same key creates nothing")
			return func(ctx context.Context, _ []string, s streams) int {
				const name = "work enqueue"
				var item any
				if *file != "" {
					given := []string{}
					fs.Visit(func(f *flag.Flag) {
						if f.Name != "file" && f.Name != "server" && f.Name != "output" && f.Name != "idempotency-key" {
							given = append(given, "--"+f.Name)
						}
					})
					if len(given) > 0 {
						return usageError(s, name, "--file holds the whole item; drop "+strings.Join(given, " ")+" or put it in the file")
					}
					raw, err := readItem(*file, s.in)
					if err != nil {
						fmt.Fprintf(s.err, "factory %s: %v\n", name, err)
						return 1
					}
					item = raw
				} else {
					if strings.TrimSpace(*title) == "" {
						return usageError(s, name, "give the item a --title, or pass --file with its JSON")
					}
					if *state != "" && *state != work.Design && *state != work.Ready {
						return usageError(s, name, fmt.Sprintf("--state %q: new work starts in design or ready", *state))
					}
					item = map[string]any{
						"title": *title, "body": *body, "repos": splitList(*repos), "source": *source,
						"external_id": *externalID, "external_url": *externalURL, "state": *state,
						"priority": *priority, "blocked_by": splitList(*blockedBy), "requires": splitList(*requires),
					}
				}
				c, code := cf.connect(s, name)
				if c == nil {
					return code
				}
				res, err := c.Enqueue(ctx, item, *key)
				if err != nil {
					return c.fail(s, err, olderServer)
				}
				if c.json {
					return printJSON(s, res)
				}
				switch {
				case res.Created:
					fmt.Fprintf(s.out, "Enqueued %s: %s (%s)\n", res.Item.ID, res.Item.Title, res.Item.State)
				case res.Changed:
					fmt.Fprintf(s.out, "Already enqueued %s: %s (%s); title and body refreshed\n", res.Item.ID, res.Item.Title, res.Item.State)
				default:
					fmt.Fprintf(s.out, "Already enqueued %s: %s (%s); left as it was: %s\n", res.Item.ID, res.Item.Title, res.Item.State, res.Reason)
				}
				return 0
			}
		},
	}
}

// readItem reads a JSON item from path, or from in for "-".
func readItem(path string, in io.Reader) (json.RawMessage, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(in)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read --file: %w. Point it at a JSON file, or - for stdin", err)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("--file %s is not JSON; fix it to hold one JSON object with the fields in docs/api.md", path)
	}
	return raw, nil
}

func workMoveCommand() command {
	return command{
		name:    "work move",
		args:    "<id> <state>",
		summary: "Ask to move a work item to another state.",
		about: `Ask the server to move a work item to another state. The state machine
decides; when it refuses, the states the item may move to are printed.`,
		env: clientEnv,
		setup: func(fs *flag.FlagSet) runner {
			cf := addClientFlags(fs)
			reason := fs.String("reason", "", "why, recorded on the item's timeline")
			return func(ctx context.Context, args []string, s streams) int {
				const name = "work move"
				id, to := args[0], args[1]
				if !slices.Contains(work.States(), to) {
					return usageError(s, name, fmt.Sprintf("%q is not a state; use one of %s", to, strings.Join(work.States(), ", ")))
				}
				c, code := cf.connect(s, name)
				if c == nil {
					return code
				}
				item, err := c.Transition(ctx, id, to, *reason)
				var apiErr *apiclient.Error
				switch {
				case errors.As(err, &apiErr) && apiErr.Code == "illegal_transition":
					fmt.Fprintf(s.err, "factory %s: %s cannot move from %s to %s. From %s it may move to: %s.\n"+
						"Run 'factory work move %s <state>' with one of those.\n",
						name, id, apiErr.CurrentState, to, apiErr.CurrentState, strings.Join(apiErr.Allowed, ", "), id)
					return 1
				case errors.As(err, &apiErr) && (apiErr.Code == "unfinished_dependencies" || apiErr.Code == "outputs_pending"):
					fmt.Fprintf(s.err, "factory %s: %s. Run 'factory work show %s' to see what it waits on.\n", name, apiErr.Message, id)
					return 1
				case err != nil:
					return c.fail(s, err, findItem)
				}
				if c.json {
					return printJSON(s, item)
				}
				fmt.Fprintf(s.out, "Moved %s to %s.\n", item.ID, item.State)
				return 0
			}
		},
	}
}
