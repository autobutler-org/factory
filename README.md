# factory

factory is the autobutler-org software factory. It takes work from any tracker, records it and its state in
Postgres, hands it to agent workers running in our Kubernetes cluster, and shows what is happening at every
moment. It ships as one Go binary, `factory`.

It is early. The plan and its issues are in the epic, [#1](https://github.com/autobutler-org/factory/issues/1).

## Development

```sh
make setup   # download modules and build the dev tools pinned in go.mod
make help    # list every target
make check   # format, tidy, lint, sqlc, migration, and test checks, for Go and the web UI (needs Node, web/.nvmrc)
make build   # ./build/factory, serving the web UI and the API
make serve/web   # the web UI's dev server, proxying /api to a factory serve you started
make serve/postgres && make test/db   # run the database tests against a local Postgres (needs Docker)
make test/e2e   # the worker's end-to-end tests, against the same Postgres
```

Run the server against a migrated database with `factory migrate up` then `factory serve`, and a worker with
`factory worker --url ...`; [docs/config.md](docs/config.md) lists the settings and [docs/api.md](docs/api.md) the
routes. The server serves the web UI on every other path: a board of work by state, each item with its step graph
and timeline, and the workers. It asks for `FACTORY_API_TOKEN` once per tab and refreshes every 5 seconds until
**Pause updates** in the header stops it. [docs/operations.md](docs/operations.md) covers the container images and installing the Helm chart in
`deploy/chart`.

See [AGENTS.md](AGENTS.md) for the layout and conventions, and [CONTRIBUTING.md](CONTRIBUTING.md) for how to
open a pull request.

## License

[MIT No Attribution](LICENSE).
