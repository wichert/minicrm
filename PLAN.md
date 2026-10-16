# Local Development Dummy OIDC Identity Server Plan

## Context

For local development and testing, MiniCRM requires an OpenID Connect (OIDC) identity server configured via `MINICRM_OIDC_ISSUER_URL`, `MINICRM_OIDC_CLIENT_ID`, `MINICRM_OIDC_CLIENT_SECRET`, and `MINICRM_OIDC_REDIRECT_URL`. Rather than bypassing OAuth2/OIDC in MiniCRM or requiring a heavy external identity server (Keycloak, Dex, etc.), we will provide a lightweight, zero-dependency standalone dummy OIDC server (`cmd/dev-oidc`, backed by `internal/auth/devidm`) that implements OIDC Discovery, JWKS, and OAuth2 Authorization Code + PKCE (`S256`) and automatically issues valid RS256-signed ID tokens for fixed credentials.

This keeps MiniCRM's production configuration and authentication pipeline completely untouched while exercising 100% of `auth.NewOIDCProvider` (`coreos/go-oidc/v3` + `golang.org/x/oauth2`), PKCE verification, session cookies, user provisioning, and audit logging.

---

## Approach

### 1. Reusable Dummy OIDC Provider Package (`internal/auth/devidm/server.go`)
- Generates an ephemeral 2048-bit RSA keypair on startup using stdlib `crypto/rsa` and exposes it as a `go-jose/v4` `JSONWebKeySet` (`kid = "dev-key-1"`, `alg = "RS256"`, `use = "sig"`). Note: `github.com/go-jose/v4` is already present in `go.mod` via `coreos/go-oidc/v3`.
- Configurable via `devidm.Config`:
  - `IssuerURL` (e.g., `http://127.0.0.1:9000`)
  - `ClientID` (optional check; accepts any client ID if empty, or validates against configured `ClientID`)
  - `ClientSecret` (optional check)
  - `Subject` (default `"dev-admin-sub"`)
  - `Email` (default `"admin@example.com"`)
  - `Name` (default `"Dev Admin"`)
- Implements four standard endpoints:
  1. **`GET /.well-known/openid-configuration`**:
     Returns the OIDC Discovery document pointing to `{IssuerURL}/authorize`, `{IssuerURL}/token`, and `{IssuerURL}/jwks`.
  2. **`GET /jwks`**:
     Returns the public `jose.JSONWebKeySet` so `coreos/go-oidc/v3` can verify ID token signatures.
  3. **`GET /authorize`**:
     - Reads `client_id`, `redirect_uri`, `state`, `nonce`, `code_challenge`, and `code_challenge_method`.
     - Validates `redirect_uri` is a valid URL and `code_challenge_method` is `"S256"` (when `code_challenge` is provided).
     - Generates a single-use random authorization `code`, stores `{clientID, redirectURI, nonce, codeChallenge, expiresAt}` in an in-memory mutex-protected map, and immediately redirects (`302 Found`) to `redirect_uri?code=<code>&state=<state>`.
  4. **`POST /token`**:
     - Parses `grant_type=authorization_code`, `code`, `redirect_uri`, `code_verifier`, and `client_id` (from POST form or HTTP Basic Auth).
     - Pops the single-use authorization code record, checks expiration, and verifies PKCE `S256` (`base64RawURL(sha256(code_verifier)) == code_challenge`).
     - Signs an RS256 JWT `id_token` containing standard OIDC claims (`iss`, `sub`, `aud`, `iat`, `exp`, `nonce`) plus profile claims (`email`, `email_verified: true`, `name`).
     - Returns JSON `{"access_token": "...", "token_type": "Bearer", "expires_in": 3600, "id_token": "<jwt>"}`.

### 2. Standalone CLI Binary (`cmd/dev-oidc/main.go`)
- Runs the `devidm.Server` on a configurable listen address with flags / environment variables:
  - `-addr` / `DEV_OIDC_ADDR` (default `127.0.0.1:9000`)
  - `-issuer` / `DEV_OIDC_ISSUER_URL` (default `http://127.0.0.1:9000`)
  - `-sub` / `DEV_OIDC_SUB` (default `dev-admin-sub`)
  - `-email` / `DEV_OIDC_EMAIL` (default `admin@example.com`)
  - `-name` / `DEV_OIDC_NAME` (default `Dev Admin`)
- Logs startup configuration and prints the exact `MINICRM_OIDC_*` environment variables to use with `minicrm`.

### 3. End-to-End Test Suite (`e2e/auth_e2e_test.go`) & GitHub Actions CI (`e2e` Job)
- **Unit/Integration Test (`internal/auth/devidm/server_test.go`)**:
  - Starts `httptest.NewServer(idm.Handler())`, initializes the real `auth.NewOIDCProvider(ctx, srv.URL, "minicrm", "secret", callbackURL)`, runs the full HTTP `/auth/login` -> `devidm /authorize` -> `/auth/callback` (which calls `devidm /token` and `devidm /jwks`) flow, and verifies that the user is authenticated, provisioned, and logged in `audit_logs`.
- **End-to-End Test Suite (`e2e/auth_e2e_test.go`)**:
  - First test in MiniCRM's end-to-end test suite (`package e2e_test`).
  - Connects to a live running `minicrm` instance (`MINICRM_E2E_BASE_URL`, e.g. `http://127.0.0.1:8080`) backed by the running `dev-oidc` service (`http://127.0.0.1:9000`) and PostgreSQL:
    1. Uses an `http.Client` with a `cookiejar.Jar` to `GET /threads`.
    2. Automatically follows the OIDC redirect chain: `minicrm /threads` -> `minicrm /auth/login` -> `dev-oidc /authorize` -> `minicrm /auth/callback` -> `minicrm /threads`.
    3. Asserts `200 OK` on `/threads`, verifies the authenticated user's display name and email are rendered in the HTML navbar, and verifies `/settings` is accessible (`200 OK`).
    4. Queries PostgreSQL `audit_logs` to verify that `user_login` was recorded for the authenticated session.
- **GitHub Actions CI Workflow (`.github/workflows/ci.yml`)**:
  - Add an `e2e` job (running with a `postgres:16-alpine` service container) that:
    1. Builds both `./cmd/dev-oidc` and `./cmd/minicrm`.
    2. Starts `dev-oidc` in the background on `127.0.0.1:9000` and waits until `http://127.0.0.1:9000/.well-known/openid-configuration` responds `200 OK`.
    3. Starts `minicrm` in the background on `127.0.0.1:8080` pointed at `MINICRM_OIDC_ISSUER_URL=http://127.0.0.1:9000` and PostgreSQL, and waits until `http://127.0.0.1:8080/auth/login` is reachable.
    4. Runs `MINICRM_E2E_BASE_URL=http://127.0.0.1:8080 go test -v -race ./e2e/...` to verify MiniCRM authenticates end-to-end against the running `dev-oidc` service.

---

## Files to Modify / Create

- `internal/auth/devidm/server.go` (new — dummy OIDC + PKCE server implementation)
- `internal/auth/devidm/server_test.go` (new — unit/integration test with `auth.NewOIDCProvider`)
- `cmd/dev-oidc/main.go` (new — standalone CLI binary for local development and CI E2E service)
- `e2e/auth_e2e_test.go` (new — end-to-end test suite verifying MiniCRM authentication against the running `dev-oidc` service)
- `.github/workflows/ci.yml` (add `e2e` CI job running `dev-oidc` and `minicrm` as services and executing `./e2e/...`)
- `go.mod` (promote `github.com/go-jose/v4` from indirect to direct requirement)
- `README.md` (document local development with `cmd/dev-oidc` and running E2E tests)
- `docs/architecture.md` (document `cmd/dev-oidc`, `internal/auth/devidm`, and the E2E test architecture)

## Reuse

- `github.com/go-jose/v4` and `github.com/go-jose/v4/jwt` (already in `go.mod` / `go.sum`)
- `internal/auth/oidc.go` (`auth.NewOIDCProvider`, `auth.Service.HandleLogin`, `auth.Service.HandleCallback`)

---

## Steps

- [x] **Step 1: Implement `internal/auth/devidm`** — Build the OIDC discovery, JWKS, `/authorize` (auto-redirect with fixed credentials + PKCE S256 capture), and `/token` (PKCE S256 verification + RS256 ID token signing) handler in `internal/auth/devidm/server.go`, plus unit tests in `internal/auth/devidm/server_test.go`.
- [x] **Step 2: Implement `cmd/dev-oidc` CLI** — Create `cmd/dev-oidc/main.go` with flags/env vars for listen address, issuer URL, and fixed user credentials (`sub`, `email`, `name`).
- [x] **Step 3: Create End-to-End Test Suite (`e2e/`) & CI Workflow** — Create `e2e/auth_e2e_test.go` and update `.github/workflows/ci.yml` to run `dev-oidc` and `minicrm` as background services and verify end-to-end OIDC authentication against PostgreSQL.
- [x] **Step 4: Update Documentation & Verify** — Document `cmd/dev-oidc` and E2E testing in `README.md` and `docs/architecture.md`, and verify `golangci-lint run ./...` and `go test -v -race ./...` pass.

## Verification

- Run `go test -v -race ./...` to verify unit/integration tests and E2E tests compile and pass.
- Run `golangci-lint run ./...` to ensure zero lint or `gosec` issues.
- Run `go build ./cmd/dev-oidc` and `go build ./cmd/minicrm` to verify both binaries compile cleanly.
- Verify `.github/workflows/ci.yml` starts `dev-oidc` + `minicrm` against PostgreSQL 16 and runs `go test -v -race ./e2e/...`.
