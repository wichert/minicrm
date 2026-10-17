# Testing, Linting & End-to-End CI Setup

MiniCRM enforces code quality, static security analysis, and functional correctness through three layers of automated tests (`unit`, `integration`, and `e2e`), local git hooks (`prek`), and a three-job GitHub Actions CI pipeline (`.github/workflows/ci.yml`).

---

## 1. Local Development & Linting (`golangci-lint` + `prek`)

### Static Analysis (`golangci-lint`)
Configured in [`.golangci.yml`](../.golangci.yml) (v2 format). Enabled linters include:
- `errcheck`, `errorlint`, `govet`, `staticcheck`, `ineffassign`, `unused`, `revive`, `gocritic`
- `gosec` (security scanner checking for SQL injection, path traversal, integer overflow, unbounded form parsing, open redirects, unsafe HTML templates, and weak TLS settings)
- `bodyclose` and `noctx` (HTTP response leak and context propagation checks)
- `gofmt` formatter check

Run locally with:
```bash
golangci-lint run ./...
```

### Local Git Hooks (`prek`)
Configured in [`.pre-commit-config.yaml`](../.pre-commit-config.yaml). Before every commit, `prek` runs:
1. `golangci-lint run ./...`
2. `go test -race ./...`

Run manually across the repository with:
```bash
prek run --all-files
```

---

## 2. Package Unit & Integration Tests

Every internal package has its own `-race`-enabled test suite:

| Package | Test File | What It Verifies |
|---|---|---|
| `internal/config` | `config_test.go` | Environment variable parsing, defaults, and `time.LoadLocation` validation for `MINICRM_DISPLAY_TIMEZONE`. |
| `internal/db/sqlc` | `tracer_test.go` | `StructuredQueryTracer` classification of `SELECT` (`data_access`) vs. `INSERT`/`UPDATE`/`DELETE` (`data_change`) with actor and request IDs. |
| `internal/storage` | `attachments_test.go` | Atomic temp-file write, SHA-256 content-addressed deduplication, path traversal rejection, and structured blob access/change logs. |
| `internal/audit` | `logger_test.go` | Recording immutable `audit_logs` and `thread_events` rows alongside structured `log/slog` output. |
| `internal/auth` | `auth_test.go` | Bootstrap admin seeding, admin user pre-creation (`user_precreated`), OIDC login binding `oidc_sub` (`user_updated` + `user_login`), and HTTP `/auth/login` -> `/auth/callback` session creation. |
| `internal/auth/devidm` | `server_test.go` | Real `auth.NewOIDCProvider` (`coreos/go-oidc/v3` + `golang.org/x/oauth2` PKCE `S256`) authenticating against `devidm.Server` (`/authorize`, `/token`, `/jwks`). |
| `internal/assignment` | `engine_test.go` | Chain-of-responsibility resolver evaluation and `DefaultAssigneeResolver` fallback. |
| `internal/threading` | `matcher_test.go` | Subject normalization (`Re:`, `Fwd:`, `AW:`), `In-Reply-To`/`References` matching, subject+sender fallback, and `reopens_thread_id` creation when replying to a `completed` thread. |
| `internal/workflow` | `service_test.go` | Valid (`new -> in_progress -> completed`, `new -> spam`) and invalid state transitions, `move_to_archive` / `move_to_junk` job queueing, and transactional `ReopenAndMerge`. |
| `internal/mail` | `mime_parser_test.go`, `imap_workers_test.go`, `smtp_sender_test.go` | Multipart MIME parsing, `bluemonday` HTML sanitization, `INBOX` UID sync, moving completed emails to `Archive`, verifying external deletion from `Archive` does not break MiniCRM or `ReopenAndMerge`, and outbound SMTP + `Sent` `APPEND`. |
| `internal/web` | `web_test.go` | `Content-Security-Policy` and all HTTP security headers on main routes and `/emails/{id}/body`, `credentialless` `<iframe sandbox>`, `display_timezone` rendering, and Reopen & Merge banner rendering. |

Run all package tests with:
```bash
go test -v -race ./...
```

---

## 3. End-to-End Test Suite (`e2e/`)

Located in [`e2e/auth_e2e_test.go`](../e2e/auth_e2e_test.go).

### How the E2E Suite Operates
`TestEndToEndOIDCAuthenticationWithDevIDM` supports two execution modes:

1. **Live Multi-Service Mode (CI & Local Staging — when `MINICRM_E2E_BASE_URL` is set)**:
   - Connects over HTTP to a running `minicrm` process (e.g., `http://127.0.0.1:8080`) that is configured against a running `dev-oidc` process (`http://127.0.0.1:9000`) and a live PostgreSQL database (`MINICRM_DATABASE_URL`).
   - Uses an `http.Client` with a `cookiejar.Jar` to request `GET /threads` unauthenticated.
   - Follows the full browser redirect chain across both services:
     1. `GET http://127.0.0.1:8080/threads` -> `303 See Other` to `/auth/login`
     2. `GET http://127.0.0.1:8080/auth/login` -> `302 Found` to `http://127.0.0.1:9000/authorize?...` (with PKCE `code_challenge`, `state`, `nonce`)
     3. `GET http://127.0.0.1:9000/authorize` -> `302 Found` to `http://127.0.0.1:8080/auth/callback?code=...&state=...`
     4. `GET http://127.0.0.1:8080/auth/callback` (MiniCRM exchanges `code` + PKCE `code_verifier` at `dev-oidc /token`, verifies the RS256 ID Token against `dev-oidc /jwks`, provisions/updates the user in PostgreSQL, records `user_login` in `audit_logs`, and sets the session cookie) -> `303 See Other` to `/threads`
     5. `GET http://127.0.0.1:8080/threads` -> `200 OK`
   - Asserts that:
     - `/threads` renders `Dev Admin (admin@example.com)` and emits the strict `Content-Security-Policy` header.
     - `/settings` responds `200 OK` using the authenticated admin session cookie.
     - PostgreSQL's `audit_logs` table contains the `user_login` event row.

2. **Self-Contained Mode (Default `go test ./...` when `MINICRM_E2E_BASE_URL` is unset)**:
   - Automatically starts ephemeral `httptest.Server` instances for both `devidm.Server` and `web.NewRouter` in-process and runs the identical HTTP redirect and session verification steps, so `go test ./...` always executes the E2E test without requiring manual background processes.

---

## 4. GitHub Actions CI Pipeline (`.github/workflows/ci.yml`)

Every `push` to `main`/`master` and every `pull_request` triggers three parallel jobs in [`.github/workflows/ci.yml`](../.github/workflows/ci.yml):

1. **`lint`**:
   - Sets up Go 1.27 and runs `golangci-lint` across the entire repository.
2. **`test`**:
   - Spins up a `postgres:16-alpine` service container and runs `go test -v -race ./...`.
3. **`e2e`**:
   - Spins up a `postgres:16-alpine` service container (`minicrm_e2e` database).
   - Compiles `./bin/dev-oidc` and `./bin/minicrm`.
   - Starts `./bin/dev-oidc` on `127.0.0.1:9000` and waits until `http://127.0.0.1:9000/.well-known/openid-configuration` is healthy.
   - Starts `./bin/minicrm` on `127.0.0.1:8080` (which runs Goose database migrations and seeds the bootstrap admin + system settings on startup) and waits until `http://127.0.0.1:8080/static/app.css` responds `200 OK`.
   - Executes `MINICRM_E2E_BASE_URL=http://127.0.0.1:8080 go test -v -race ./e2e/...` to verify end-to-end authentication against the live services and PostgreSQL database.
