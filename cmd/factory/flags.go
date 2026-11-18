package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// parseFlags parses args into fs, then gives every flag that was not on the
// command line the value of its FACTORY_<FLAG> environment variable, if set:
// --stuck-after reads FACTORY_STUCK_AFTER. A flag always wins over the
// environment.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if given[f.Name] || err != nil {
			return
		}
		name := envName(f.Name)
		if value, ok := os.LookupEnv(name); ok && value != "" {
			if setErr := fs.Set(f.Name, value); setErr != nil {
				err = fmt.Errorf("%s=%q: %w", name, value, setErr)
			}
		}
	})
	return err
}

func envName(flagName string) string {
	return "FACTORY_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}
