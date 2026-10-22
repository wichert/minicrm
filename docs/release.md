# Release Process

MiniCRM uses [Conventional Commits](https://www.conventionalcommits.org/) for
commit messages and [release-please](https://github.com/googleapis/release-please)
plus a tag-triggered release workflow to publish GitHub releases and Docker
images to GitHub Packages (ghcr.io).

## Commit message convention

Every commit message must follow the Conventional Commits format:

```
<type>(<optional scope>)!: <description>

[optional body]

[optional footer(s)]
```

- Common types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `ci`,
  `build`, `perf`, `style`.
- `feat` bumps the minor version, `fix` bumps the patch version.
- Add `!` after the type (or a `BREAKING CHANGE: <description>` footer) for
  breaking changes; this bumps the major version.

Examples:

```
feat(threading): reopen completed threads as new threads
fix(imap): preserve Message-ID when appending to Sent
refactor!: replace sqlite storage with PostgreSQL
```

This convention is enforced:

- **Locally** — a `commitlint` hook in `.pre-commit-config.yaml` (run via
  `prek`) validates the commit message on every commit.
- **CI** — a `commitlint` job in `.github/workflows/ci.yml` validates all
  commits on every pull request.

## How a release happens

1. **Develop & merge.** Contributors commit using the convention above; PRs
   run lint, tests, e2e, and commitlint.
2. **Release PR.** `release-please` (`.github/workflows/release-please.yml`)
   accumulates commits on `main` and opens (or updates) a release PR that
   bumps the version and updates `CHANGELOG.md`.
3. **Tag & release.** Merging the release PR creates tag `vX.Y.Z` and a
   GitHub release whose notes categorize the commits by type (Features,
   Bug Fixes, etc.).
4. **Test gate.** Pushing the tag triggers `.github/workflows/release.yml`,
   which runs the shared `.github/workflows/tests.yml` reusable workflow
   (golangci-lint, unit/integration tests, e2e tests) against the exact tag
   commit.
5. **Publish image.** Only if all tests pass, the Docker image is built
   (multi-platform: `linux/amd64` + `linux/arm64`) and pushed to
   `ghcr.io/wichert/minicrm` with tags `X.Y.Z`, `X.Y`, and `X`. The image
   digest is recorded in the workflow summary.

If any test fails on the tag, no image is published — the release notes and
tag exist, but users are never offered an untested image.

## Shared test workflow

`ci.yml` and `release.yml` both call `.github/workflows/tests.yml`
(`workflow_call`), so the same lint/test/e2e suite gates pull requests and
releases without duplication.
