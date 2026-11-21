package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/work"
)

// findItem is the next step when an item is not found.
const findItem = "Run 'factory work list' to find an item's ID."

func workListCommand() command {
	return command{
		name:    "work list",
		summary: "List work items, highest priority first, then oldest.",
		about: `List work items, highest priority first, then oldest first. When more
items match than --limit, the next page's cursor is printed on stderr.`,
		env: clientEnv,
		setup: func(fs *flag.FlagSet) runner {
			cf := addClientFlags(fs)
			state := fs.String("state", "", "only items in this `state`: "+strings.Join(work.States(), ", "))
			source := fs.String("source", "", "only items from this `source`, such as github")
			repo := fs.String("repo", "", "only items that change this `repository` (owner/name)")
			claimedBy := fs.String("claimed-by", "", "only items claimed by this worker `ID`")
			limit := fs.Int("limit", 0, "at most this many items; the server's default is 50 and its most 200")
			cursor := fs.String("cursor", "", "the `cursor` of the page to list, printed by the page before it")
			return func(ctx context.Context, _ []string, s streams) int {
				if *state != "" && !slices.Contains(work.States(), *state) {
					return usageError(s, "work list", fmt.Sprintf("--state %q is not a state; use one of %s", *state, strings.Join(work.States(), ", ")))
				}
				c, code := cf.connect(s, "work list")
				if c == nil {
					return code
				}
				q := url.Values{}
				for k, v := range map[string]string{"state": *state, "source": *source, "repo": *repo, "claimed_by": *claimedBy, "cursor": *cursor} {
					if v != "" {
						q.Set(k, v)
					}
				}
				if *limit != 0 {
					q.Set("limit", strconv.Itoa(*limit))
				}
				page, err := c.ListWork(ctx, q)
				if err != nil {
					return c.fail(s, err, olderServer)
				}
				if c.json {
					return printJSON(s, page)
				}
				if len(page.Items) == 0 {
					fmt.Fprintln(s.out, "No work items match.")
					return 0
				}
				t := newTable(s.out)
				fmt.Fprintln(t, "ID\tSTATE\tPRIORITY\tREPOS\tTITLE")
				for _, it := range page.Items {
					fmt.Fprintf(t, "%s\t%s\t%d\t%s\t%s\n", it.ID, it.State, it.Priority, strings.Join(it.Repos, ","), it.Title)
				}
				_ = t.Flush()
				if page.NextCursor != "" {
					fmt.Fprintf(s.err, "More items match: run again with --cursor %s\n", page.NextCursor)
				}
				return 0
			}
		},
	}
}

func workShowCommand() command {
	return command{
		name:    "work show",
		args:    "<id>",
		summary: "Show one work item with its step graph and event timeline.",
		about:   "Show one work item, its latest step graph, and its event timeline, oldest first.",
		env:     clientEnv,
		setup: func(fs *flag.FlagSet) runner {
			cf := addClientFlags(fs)
			return func(ctx context.Context, args []string, s streams) int {
				c, code := cf.connect(s, "work show")
				if c == nil {
					return code
				}
				id := args[0]
				item, err := c.Work(ctx, id)
				if err != nil {
					return c.fail(s, err, findItem)
				}
				graph, err := c.Graph(ctx, id)
				if err != nil {
					return c.fail(s, err, olderServer)
				}
				events, err := c.Events(ctx, id)
				if err != nil {
					return c.fail(s, err, olderServer)
				}
				if c.json {
					return printJSON(s, map[string]any{"item": item, "graph": graph, "events": events})
				}
				printItem(s, item)
				printGraph(s, graph)
				printTimeline(s, events)
				return 0
			}
		},
	}
}

func printItem(s streams, it db.WorkItem) {
	t := newTable(s.out)
	state := it.State
	if it.StateReason != "" {
		state += " (" + it.StateReason + ")"
	}
	rows := [][2]string{
		{"ID", it.ID},
		{"Title", it.Title},
		{"State", state},
		{"Stage", it.Stage},
		{"Priority", strconv.Itoa(int(it.Priority))},
		{"Repos", strings.Join(it.Repos, ", ")},
		{"Source", strings.TrimSpace(deref(it.Source) + " " + deref(it.ExternalID))},
		{"URL", deref(it.ExternalUrl)},
		{"Requires", strings.Join(it.Requires, ", ")},
		{"Claimed by", deref(it.ClaimedBy)},
		{"Attempts", strconv.Itoa(int(it.Attempts))},
		{"Created", when(it.CreatedAt)},
		{"Updated", when(it.UpdatedAt)},
	}
	for _, r := range rows {
		if r[1] != "" {
			fmt.Fprintf(t, "%s:\t%s\n", r[0], r[1])
		}
	}
	_ = t.Flush()
	if body := strings.TrimSpace(it.Body); body != "" {
		fmt.Fprintf(s.out, "\n%s\n", body)
	}
}

func printGraph(s streams, g *work.GraphView) {
	if g == nil {
		fmt.Fprintln(s.out, "\nNot planned yet: no step graph.")
		return
	}
	fmt.Fprintf(s.out, "\nSteps (graph v%d, %s, %s):\n", g.Graph.Version, g.Graph.Template, g.Graph.State)
	keys := map[string]string{}
	for _, st := range g.Steps {
		keys[st.ID] = st.Key
	}
	deps := map[string][]string{}
	for _, e := range g.Edges {
		deps[e.StepID] = append(deps[e.StepID], keys[e.DependsOnID])
	}
	t := newTable(s.out)
	fmt.Fprintln(t, "  KEY\tKIND\tSTATE\tATTEMPTS\tAFTER")
	for _, st := range g.Steps {
		fmt.Fprintf(t, "  %s\t%s\t%s\t%d\t%s\n", st.Key, st.Kind, st.State, st.Attempts, strings.Join(deps[st.ID], ","))
	}
	_ = t.Flush()
}

func printTimeline(s streams, events []db.WorkEvent) {
	fmt.Fprintln(s.out, "\nTimeline:")
	t := newTable(s.out)
	for _, e := range events {
		what := e.Type
		if e.FromState != nil || e.ToState != nil {
			what = strings.Join(strings.Fields(e.Type+" "+deref(e.FromState)+" -> "+deref(e.ToState)), " ")
		}
		var payload struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(e.Payload, &payload)
		if payload.Reason != "" {
			what += ": " + payload.Reason
		}
		fmt.Fprintf(t, "  %s\t%s %s\t%s\n", when(e.At), e.ActorKind, e.ActorID, what)
	}
	_ = t.Flush()
}
