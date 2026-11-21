package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"text/tabwriter"
	"time"

	"github.com/autobutler-org/factory/internal/apiclient"
)

// serverEnv names the server when --server is not given.
const serverEnv = "FACTORY_SERVER"

// clientEnv is the Environment section of every command that calls the API.
//
// ponytail: the token comes from the environment only, so it stays out of
// process listings; #15 adds factory login and a config file it writes.
const clientEnv = `  FACTORY_SERVER       the server's URL when --server is not given
  FACTORY_API_TOKEN    the API token the server was started with`

// clientFlags are the flags of every command that calls the API.
type clientFlags struct {
	server, output *string
}

func addClientFlags(fs *flag.FlagSet) clientFlags {
	return clientFlags{
		server: fs.String("server", "", "the factory server's `URL`, such as http://127.0.0.1:8080 (default $FACTORY_SERVER)"),
		output: fs.String("output", "table", "output `format`: table or json"),
	}
}

// client is a command's connection to the server.
type client struct {
	*apiclient.Client
	name string // the command, for error messages
	json bool   // --output json
}

// connect checks the flags and returns the client. On a usage error it
// prints why and returns nil and the exit code.
func (f clientFlags) connect(s streams, name string) (*client, int) {
	if *f.output != "table" && *f.output != "json" {
		return nil, usageError(s, name, fmt.Sprintf("--output %q: use table or json", *f.output))
	}
	server := *f.server
	if server == "" {
		server = os.Getenv(serverEnv)
	}
	if server == "" {
		return nil, usageError(s, name, "no server: pass --server or set FACTORY_SERVER to its URL, such as http://127.0.0.1:8080")
	}
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, usageError(s, name, fmt.Sprintf("server %q is not an http or https URL; use one such as http://127.0.0.1:8080", server))
	}
	token := os.Getenv(apiTokenEnv)
	return &client{
		Client: &apiclient.Client{URL: server, Token: func() (string, error) { return token, nil }},
		name:   name,
		json:   *f.output == "json",
	}, 0
}

// fail prints err with what to do about it and returns 1. notFound is the
// next step for a 404.
func (c *client) fail(s streams, err error, notFound string) int {
	fmt.Fprintf(s.err, "factory %s: %s\n", c.name, c.explain(err, notFound))
	return 1
}

func (c *client) explain(err error, notFound string) string {
	var apiErr *apiclient.Error
	switch {
	case errors.Is(err, apiclient.ErrUnreachable):
		return fmt.Sprintf("cannot reach the factory server at %s. Check --server or FACTORY_SERVER, and that the server is running.", c.URL)
	case !errors.As(err, &apiErr):
		return err.Error()
	case apiErr.Code == "":
		return fmt.Sprintf("the server answered %d with no factory error. Check that --server or FACTORY_SERVER points at a factory server; run 'factory doctor'.", apiErr.Status)
	case apiErr.Status == http.StatusUnauthorized && os.Getenv(apiTokenEnv) == "":
		return "the server needs an API token: set FACTORY_API_TOKEN to the token it was started with."
	case apiErr.Status == http.StatusUnauthorized:
		return "the server refused FACTORY_API_TOKEN: set it to the token the server was started with."
	case apiErr.Status == http.StatusNotFound:
		return apiErr.Message + ". " + notFound
	case apiErr.Code == "internal":
		return fmt.Sprintf("the server failed: check its log for request ID %s, then retry.", apiErr.RequestID)
	}
	return fmt.Sprintf("the server refused: %s (%s). Run 'factory doctor' if the cause is not clear.", apiErr.Message, apiErr.Code)
}

// olderServer is the next step for a 404 from a route this binary expects.
const olderServer = "The server may be older than this factory; run 'factory doctor' to compare versions."

// printJSON writes v as indented JSON to stdout and returns the exit code.
func printJSON(s streams, v any) int {
	enc := json.NewEncoder(s.out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(s.err, "factory: write the output: %v\n", err)
		return 1
	}
	return 0
}

func newTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

// when formats a time for a table, in UTC.
func when(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

// deref returns *p, or "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
