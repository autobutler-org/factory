# Contributing

## Before you start

Set your git `user.name` and `user.email` to match your GitHub profile. Every commit is signed off
(`git commit -s`).

Run `make setup` once, then `make help` to see the targets. [AGENTS.md](AGENTS.md) has the layout and the
coding conventions; they apply to people and agents alike.

## The workflow

1. Pick or open an issue. One issue becomes one pull request.
2. Branch off `main` as `<issue number>_short-description`, for example `12_claim-lease`.
3. Write the failing test first, then the code.
4. Run `make format`, `make tidy`, and `make check`, and commit what changed.
5. Open a pull request and fill in the template, including `Closes #N`.

## Pull request titles

Titles are conventional commits in lowercase imperative mood, with the issue number as the scope:

```text
feat(12): add claim lease
fix(31): release the lease when a worker disconnects
docs(40): describe the work states
```

The type is one of `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, or
`revert`. The `pr-title` check fails any other title. Pull requests are squash merged, so the title becomes
the commit subject on `main`.

Do not put AI attribution in a title, description, or commit message. The `pr-title` workflow rejects it.

## Found a bug or have an idea?

[Open an issue](https://github.com/autobutler-org/factory/issues/new/choose). Report a vulnerability
privately, as [SECURITY.md](SECURITY.md) describes.
