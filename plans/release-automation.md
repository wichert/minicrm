# Release Automation Plan

Goal: pushing a version tag (`v*`, e.g. `v1.2.3`) to `main` produces a GitHub
release with auto-generated release notes and a Docker image published to
GitHub Packages (ghcr.io) — but only if all tests pass.

## 1. Commit message convention (Conventional Commits)

Release notes generated "from commits" need a standard. Adopt
[Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`,
`docs:`, `refactor:`, `chore:`, `test:`, ...), which is what all the popular
changelog generators understand.

Changes:
- `docs/` — add a short "Commit & release conventions" section to
  `README.md` **and** re-emphasize the required format in `AGENTS.md`
  (`feat(scope): description`, `BREAKING CHANGE:` footer, `!` for breaking).
- Use **commitlint** itself (npm `@commitlint/cli` with the
  `@commitlint/config-conventional` preset) — do not reimplement the
  validation. Wire it up as:
  - a `commitlint` config (`commitlint.config.js` with
    `extends: ['@commitlint/config-conventional']`),
  - a CI job running `npx --no-install commitlint --from ${{ github.event.pull_request.base.sha }} --to HEAD`
    on pull requests,
  - a local hook via **prek** (the repo's existing pre-commit framework): add
    a `commit-msg` stage entry in `.pre-commit-config.yaml` running
    commitlint on the message file, so bad messages are caught before they
    are ever pushed.

## 2. Release workflow: `.github/workflows/release.yml`

Trigger:

```yaml
on:
  push:
    tags: ["v*"]
```

Jobs (single job, sequential steps, so "tests pass" gates everything):

1. **Checkout + Go 1.27 setup** (same as CI).
2. **Lint** — provided by the shared `tests.yml` workflow.
3. **Unit & integration tests** — provided by the shared `tests.yml`
   workflow (same Postgres service container as `ci.yml`).
4. **E2E tests** — provided by the shared `tests.yml` workflow
   (dev-oidc + minicrm + Postgres). This job is the gate: if anything fails,
   later steps never run and no release/image is published.
   - Alternative considered: `workflow_run` waiting on CI. Rejected — racy
     and harder to reason about. Calling the shared `tests.yml` on the exact
     tag commit is deterministic *and* avoids duplication (see step 1).
1. **Reusable test job** — all CI logic is factored into a **local
   reusable workflow** (`.github/workflows/tests.yml`, declared with
   `on: workflow_call`), containing the existing lint, unit/integration,
   and e2e jobs verbatim.
   - `ci.yml` becomes a thin wrapper: `on: [push, pull_request]` →
     `uses: ./.github/workflows/tests.yml` (secrets/inputs passed
     explicitly).
   - `release.yml` calls the same `tests.yml` on the tag commit, then
     proceeds only if it succeeded — no duplicated test logic, and the
     release gate is guaranteed to run the identical test suite as CI.
   - Note: GitHub requires reusable workflows and their callers to live in
     the same repo, which is the case here.
5. **Docker build & push to ghcr.io**
   - `docker/setup-buildx-action` + `docker/login-action@v3` with
     `GITHUB_TOKEN` (scope: `packages: write`).
   - `docker/build-push-action@v6` to `ghcr.io/<owner>/minicrm`.
   - Tags: `latest` (for the default branch), semver tags from the git tag
     (`1.2.3`, `1.2`, `1`) via `docker/metadata-action@v5` so users can pin
     major/minor versions.
   - `cache-from/cache-to: gha` for speed; multi-platform
     `linux/amd64,linux/arm64` (Go is trivially cross-compilable; buildx
     QEMU handles it — verify arm64 build works in first run).
   - Push only happens after the test steps above succeeded.
6. **Create GitHub release**
   - `softprops/action-gh-release@v2` with `generate_release_notes: true`
     (GitHub's built-in notes group PRs by label, not by commit type).
   - To get **commit-type-aware** notes ("Features" / "Bug Fixes" sections
     derived from Conventional Commits), generate notes with
     `googleapis/release-please-action` (also maintains a `CHANGELOG.md`) or
     `mikepenz/release-changelog-builder-action`.
   - **Chosen approach:** `release-please-action` in a separate small
     `release-please.yml` on push to `main`: it maintains
     `CHANGELOG.md`, opens a "chore(main): release x.y.z" PR, and when that
     PR is merged it **creates the tag + GitHub release with conventional
     commit sections**. The tag push then triggers `release.yml` above,
     which builds/tests and publishes the image.
   - This means releases are created by merging release-please's PR — a
     deliberate, reviewable act; the Docker image is only published for
     tags whose commit passed the full test gate.

### Permissions

```yaml
permissions:
  contents: write        # create releases/tags (release-please)
  packages: write        # push images to ghcr.io
  pull-requests: write   # release-please release PRs
  id-token: write        # not needed unless we add cosign signing later
```

### Repo settings required (one-time, manual or via first run)

- Settings → Actions → General → Workflow permissions: allow
  "Read and write permissions" (or rely on the explicit `permissions:` block
  above, which is sufficient).
- Packages: the first push creates `ghcr.io/<owner>/minicrm`; set it public
  (or leave private) in package settings afterwards.

## 3. Files to create/modify

| File | Change |
|---|---|
| `.github/workflows/release.yml` | new — tag-triggered: calls shared `tests.yml` (lint → tests → e2e) → docker build/push to ghcr → attach image digest to release |
| `.github/workflows/tests.yml` | new — reusable (`workflow_call`) workflow holding all lint/test/e2e jobs, called by both `ci.yml` and `release.yml` |
| `.github/workflows/ci.yml` | refactor into a thin caller of `tests.yml`; add a `commitlint` job on PRs |
| `.github/workflows/release-please.yml` | new — maintains CHANGELOG.md, opens release PRs, creates tagged GitHub releases with conventional-commit notes |
| `.pre-commit-config.yaml` | add commitlint hook (commit-msg stage) to prek config |
| `commitlint.config.js` | new — commitlint config extending `@commitlint/config-conventional` |
| `README.md` **and** `AGENTS.md` | document: commit format, how to release (merge release PR), where images are published |
| `docs/release.md` | new — release design doc: commit convention, release-please flow, release.yml pipeline, image publishing; link it from the docs index in `README.md`/`AGENTS.md` |

## 4. Release flow summary

1. Developer commits with Conventional Commits messages (enforced locally by
   commitlint's hook and in CI by the `commitlint` job) → PR → CI (lint,
   commitlint, tests, e2e) must pass.
2. Merge to `main`. release-please accumulates commits and opens/updates a
   release PR bumping the version and `CHANGELOG.md`.
3. Merging that PR creates tag `vX.Y.Z` + draft→published GitHub release
   with categorized notes (Features / Bug Fixes / etc.).
4. Tag push starts `release.yml`: full lint + tests + e2e run against the
   tag. Only when green is the Docker image built and pushed to
   `ghcr.io/<owner>/minicrm:X.Y.Z` (plus `latest`), and the image digest is
   appended to the release. A failed run means **no** image is published.
