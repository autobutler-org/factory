package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/autobutler-org/factory/internal/work"
)

func workersListCommand() command {
	return command{
		name:    "workers list",
		summary: "List the worker registry and what each worker holds.",
		about:   "List the registered workers by name, each with its state and what it holds now.",
		env:     clientEnv,
		setup: func(fs *flag.FlagSet) runner {
			cf := addClientFlags(fs)
			return func(ctx context.Context, _ []string, s streams) int {
				c, code := cf.connect(s, "workers list")
				if c == nil {
					return code
				}
				workers, err := c.Workers(ctx)
				if err != nil {
					return c.fail(s, err, olderServer)
				}
				if c.json {
					return printJSON(s, map[string]any{"workers": workers})
				}
				if len(workers) == 0 {
					fmt.Fprintln(s.out, "No workers have registered.")
					return 0
				}
				t := newTable(s.out)
				fmt.Fprintln(t, "NAME\tKIND\tSTATE\tVERSION\tLAST SEEN\tHOLDS")
				for _, w := range workers {
					fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", w.Name, w.Kind, w.State, w.Version, when(w.LastSeenAt), holds(w.Holds))
				}
				_ = t.Flush()
				return 0
			}
		},
	}
}

func holds(hs []work.Hold) string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.WorkItemID
		if h.StepKey != "" {
			out[i] += " " + h.StepKey
		}
	}
	return strings.Join(out, ", ")
}

func statusCommand() command {
	return command{
		name:    "status",
		summary: "Show work counts by state, workers, the oldest ready item, and recent failures.",
		about: `Show how many work items are in each state, the workers by state, the item
that has waited in ready longest, and the latest failures.`,
		env: clientEnv,
		setup: func(fs *flag.FlagSet) runner {
			cf := addClientFlags(fs)
			return func(ctx context.Context, _ []string, s streams) int {
				c, code := cf.connect(s, "status")
				if c == nil {
					return code
				}
				st, err := c.Status(ctx)
				if err != nil {
					return c.fail(s, err, olderServer)
				}
				if c.json {
					return printJSON(s, st)
				}
				fmt.Fprintln(s.out, "Work by state:")
				t := newTable(s.out)
				for _, state := range work.States() {
					fmt.Fprintf(t, "  %s\t%d\n", state, st.Counts[state])
				}
				_ = t.Flush()
				fmt.Fprintf(s.out, "\nWorkers: %d active, %d draining, %d gone\n",
					st.Workers[work.WorkerActive], st.Workers[work.WorkerDraining], st.Workers[work.WorkerGone])
				if r := st.OldestReady; r != nil {
					fmt.Fprintf(s.out, "\nOldest ready: %s %s (ready since %s)\n", r.ID, r.Title, when(r.StateChangedAt))
				} else {
					fmt.Fprintln(s.out, "\nNothing is ready.")
				}
				if len(st.RecentFailures) == 0 {
					fmt.Fprintln(s.out, "\nNo failures.")
					return 0
				}
				fmt.Fprintln(s.out, "\nRecent failures:")
				t = newTable(s.out)
				for _, f := range st.RecentFailures {
					fmt.Fprintf(t, "  %s\t%s\t%s\t%s\n", when(f.StateChangedAt), f.ID, f.Title, f.StateReason)
				}
				_ = t.Flush()
				return 0
			}
		},
	}
}
