# factory

factory is the autobutler-org software factory. It takes work from any tracker, records it and its state in
Postgres, hands it to agent workers running in our Kubernetes cluster, and shows what is happening at every
moment. It ships as one Go binary, `factory`.

It is early. The plan and its issues are in the epic, [#1](https://github.com/autobutler-org/factory/issues/1).

## Development

```sh
make setup   # download modules and build the dev tools pinned in go.mod
make help    # list every target
make check   # format, tidy, lint, sqlc, migration, and test checks
make serve/postgres && make test/db   # run the database tests against a local Postgres (needs Docker)
```

See [AGENTS.md](AGENTS.md) for the layout and conventions, and [CONTRIBUTING.md](CONTRIBUTING.md) for how to
open a pull request.

## License

[MIT No Attribution](LICENSE).
