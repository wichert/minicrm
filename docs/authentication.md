# OAuth2 / OIDC Authentication & Dummy IDM (`dev-oidc`)

This document describes how **MiniCRM** authenticates users via OpenID Connect (OIDC), how users and pre-created assignees are provisioned and audited, and how to use the built-in dummy OIDC identity server (`cmd/dev-oidc`) for local development and automated end-to-end testing.

---

## 1. Production OAuth2 / OIDC Setup

MiniCRM uses the **OAuth2 Authorization Code Flow with PKCE (`S256`)** and **OpenID Connect (OIDC) Discovery** (`github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2`).

### Identity Provider (IdP) Requirements

Your OIDC identity server (e.g., Keycloak, Dex, Okta, Auth0, Entra ID, Authelia, Authentik) must:

1. Serve an OIDC Discovery document at `{MINICRM_OIDC_ISSUER_URL}/.well-known/openid-configuration`.
2. Publish its public signing keys at the `jwks_uri` advertised in the discovery document (`RS256` supported out of the box).
3. Support the `authorization_code` grant type and PKCE (`code_challenge_method=S256`).
4. Include the following claims in the signed `id_token` when scopes `openid profile email` are requested:
   - `sub` (unique subject identifier for the user)
   - `email` (email address, used to match pre-created users or bootstrap admin)
   - `name` (optional display name; falls back to `email` if omitted)
   - `nonce` (echoed from the `/authorize` request)

### Registering MiniCRM as an OIDC Client

In your identity server, create a confidential OAuth2/OIDC client with:

- **Grant Type**: `authorization_code`
- **Response Type**: `code`
- **PKCE**: Required / `S256`
- **Redirect URI**: `https://<your-minicrm-domain>/auth/callback`
- **Scopes**: `openid`, `profile`, `email`

### MiniCRM OIDC Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `MINICRM_OIDC_ISSUER_URL` | **Yes** | — | Exact issuer URL of the OIDC provider (must match the `iss` claim in ID tokens and serve `/.well-known/openid-configuration`). |
| `MINICRM_OIDC_CLIENT_ID` | **Yes** | — | OAuth2 Client ID registered in the IdP. |
| `MINICRM_OIDC_CLIENT_SECRET` | **Yes** | — | OAuth2 Client Secret registered in the IdP. |
| `MINICRM_OIDC_REDIRECT_URL` | **Yes** | — | Full callback URL pointing to MiniCRM's `/auth/callback` endpoint. |
| `MINICRM_BOOTSTRAP_ADMIN_EMAIL` | **Yes** | — | Email address of the initial admin user and default assignee seeded on startup. |
| `MINICRM_BOOTSTRAP_ADMIN_NAME` | No | `Admin` | Display name for the seeded bootstrap admin user. |
| `MINICRM_COOKIE_SECURE` | No | `true` | When `true` (default), session cookies use the `__Host-minicrm_session` name, `Secure`, `HttpOnly`, and `SameSite=Lax`. Set to `false` only for plain `http://localhost` development. |

---

## 2. Authentication Flow & User Provisioning

```text
Browser                     MiniCRM (/auth/*)                  OIDC Identity Server
  |                                |                                    |
  | 1. GET /threads (unauthed)     |                                    |
  |------------------------------->|                                    |
  | 2. 303 Redirect -> /auth/login |                                    |
  |<-------------------------------|                                    |
  | 3. GET /auth/login             |                                    |
  |------------------------------->|                                    |
  |    Generates state, nonce,     |                                    |
  |    PKCE code_verifier (S256)   |                                    |
  | 4. 302 Redirect -> /authorize  |                                    |
  |<-------------------------------|                                    |
  | 5. GET /authorize?client_id=...&state=...&nonce=...&code_challenge= |
  |-------------------------------------------------------------------->|
  | 6. 302 Redirect -> /auth/callback?code=...&state=...                |
  |<--------------------------------------------------------------------|
  | 7. GET /auth/callback          |                                    |
  |------------------------------->|                                    |
  |    Verifies state              | 8. POST /token (code + verifier)   |
  |                                |----------------------------------->|
  |                                | 9. Returns signed id_token (JWT)   |
  |                                |<-----------------------------------|
  |                                | 10. Verifies JWT signature (/jwks),|
  |                                |     iss, aud, exp, and nonce       |
  |                                | 11. Upserts user & logs user_login |
  |                                |     in audit_logs + RenewToken     |
  | 12. 303 Redirect -> /threads   |                                    |
  |<-------------------------------|                                    |
```

### User Resolution Order (`auth.Service.ResolveAndLoginUser`)

When a valid ID Token is verified at `/auth/callback`, MiniCRM resolves the user (`sub`, `email`, `name`) in three steps:

1. **Match by `oidc_sub`**:
   - Queries `SELECT ... FROM users WHERE oidc_sub = $1`.
   - Used for all returning users after their first login.
2. **Match by `email` (Pre-Created Users & Bootstrap Admin)**:
   - If no user matches `oidc_sub`, queries `SELECT ... FROM users WHERE email = $1` (case-insensitive `CITEXT`).
   - This matches users that were **pre-created by an admin** in `/settings` (or seeded at startup via `MINICRM_BOOTSTRAP_ADMIN_EMAIL`) so emails could be assigned to them before they ever logged in.
   - MiniCRM binds their `oidc_sub`, updates their `display_name` from the ID Token, and writes an immutable `user_updated` record to `audit_logs`.
3. **Auto-Provision New User**:
   - If neither `oidc_sub` nor `email` exists, MiniCRM creates a new user row (`role = 'agent'`, or `'admin'` if the email matches `MINICRM_BOOTSTRAP_ADMIN_EMAIL`) and writes a `user_created` record to `audit_logs`.

Finally:
- If `user.is_active == false`, MiniCRM logs `user_login_failed` in `audit_logs` and rejects the login with `403 Forbidden`.
- Otherwise, MiniCRM logs `user_login` (with client IP and User-Agent) in `audit_logs`, rotates the SCS session token (`sessions.RenewToken`) to prevent session fixation, and redirects to `/threads`.

---

## 3. Dummy OIDC Identity Server (`cmd/dev-oidc` & `internal/auth/devidm`)

For local development and CI end-to-end testing, running an external OIDC server is unnecessary overhead. MiniCRM includes a standalone, standards-compliant dummy OIDC server (`cmd/dev-oidc`, implemented in `internal/auth/devidm`) that speaks real OIDC Discovery, JWKS, PKCE `S256`, and RS256 JWT signing.

Because `dev-oidc` implements the real protocol rather than bypassing authentication inside MiniCRM, local development and CI test the exact same `auth.OIDCProvider`, PKCE verification, session cookie management, user provisioning, and audit logging code used in production.

### How `dev-oidc` Works (`internal/auth/devidm/server.go`)

1. **Ephemeral RSA Signing Key**:
   - On startup, `devidm.NewServer` generates a 2048-bit RSA keypair in memory (`kid = "dev-key-1"`, `alg = "RS256"`) and exposes the public key at `GET /jwks`.
2. **Discovery (`GET /.well-known/openid-configuration`)**:
   - Returns the standard OIDC metadata JSON pointing to `{issuer}/authorize`, `{issuer}/token`, and `{issuer}/jwks`.
3. **Zero-Click Authorization (`GET /authorize`)**:
   - Validates `client_id` (if `-client-id` is configured), `redirect_uri`, and `code_challenge_method=S256`.
   - Generates a single-use cryptographic authorization code bound to `{clientID, redirectURI, nonce, codeChallenge}` (5-minute TTL) and immediately responds with `302 Found` redirecting back to `redirect_uri?code=<code>&state=<state>`.
4. **PKCE Verification & ID Token Issuance (`POST /token`)**:
   - Consumes the single-use authorization code, verifies `client_secret` (if configured), and validates the PKCE `code_verifier` (`base64RawURL(sha256(code_verifier)) == code_challenge`).
   - Signs an RS256 JWT `id_token` containing the configured fixed user identity (`sub`, `email`, `email_verified: true`, `name`) along with `iss`, `aud`, `iat`, `exp`, and `nonce`.

### CLI Flags & Environment Variables for `cmd/dev-oidc`

| Flag | Environment Variable | Default | Description |
|---|---|---|---|
| `-addr` | `DEV_OIDC_ADDR` | `127.0.0.1:9000` | TCP address for the dummy OIDC HTTP server to listen on. |
| `-issuer` | `DEV_OIDC_ISSUER_URL` | `http://<addr>` | Public issuer URL placed in discovery metadata and the `iss` JWT claim. |
| `-client-id` | `DEV_OIDC_CLIENT_ID` | *(empty — accepts any)* | Optional expected OAuth2 `client_id`. |
| `-client-secret` | `DEV_OIDC_CLIENT_SECRET` | *(empty — accepts any)* | Optional expected OAuth2 `client_secret`. |
| `-sub` | `DEV_OIDC_SUB` | `dev-admin-sub` | Fixed `sub` claim returned in the signed ID Token. |
| `-email` | `DEV_OIDC_EMAIL` | `admin@example.com` | Fixed `email` claim returned in the signed ID Token. |
| `-name` | `DEV_OIDC_NAME` | `Dev Admin` | Fixed `name` claim returned in the signed ID Token. |

### Step-by-Step Local Development Example

1. **Start `dev-oidc` in terminal 1**:
   ```bash
   go run ./cmd/dev-oidc \
     -addr 127.0.0.1:9000 \
     -email admin@example.com \
     -name "Dev Admin"
   ```
   You can inspect its discovery document at `http://127.0.0.1:9000/.well-known/openid-configuration`.

2. **Start `minicrm` in terminal 2**:
   ```bash
   export MINICRM_HTTP_ADDR="127.0.0.1:8080"
   export MINICRM_DATABASE_URL="postgres://localhost:5432/minicrm?sslmode=disable"
   export MINICRM_BOOTSTRAP_ADMIN_EMAIL="admin@example.com"
   export MINICRM_COOKIE_SECURE="false"
   export MINICRM_IMAP_HOST="127.0.0.1"
   export MINICRM_IMAP_PORT="1993"
   export MINICRM_IMAP_USERNAME="support@example.com"
   export MINICRM_IMAP_PASSWORD="secret"
   export MINICRM_SMTP_HOST="127.0.0.1"
   export MINICRM_SMTP_PORT="1587"
   export MINICRM_SMTP_USERNAME="support@example.com"
   export MINICRM_SMTP_PASSWORD="secret"
   export MINICRM_SMTP_FROM_ADDRESS="support@example.com"
   export MINICRM_OIDC_ISSUER_URL="http://127.0.0.1:9000"
   export MINICRM_OIDC_CLIENT_ID="minicrm-dev"
   export MINICRM_OIDC_CLIENT_SECRET="minicrm-dev-secret"
   export MINICRM_OIDC_REDIRECT_URL="http://127.0.0.1:8080/auth/callback"

   go run ./cmd/minicrm
   ```

3. **Open `http://127.0.0.1:8080/threads` in your browser**:
   - MiniCRM redirects to `/auth/login` -> `http://127.0.0.1:9000/authorize` -> `/auth/callback` -> `/threads` and signs you in automatically as `Dev Admin (admin@example.com)`.
   - To test logging in as a non-admin agent, restart `dev-oidc` with `-sub dev-agent-sub -email agent@example.com -name "Test Agent"`.

---

## 4. Automated Testing with `devidm` & `e2e/`

MiniCRM tests the dummy IDM and OIDC integration at two levels:

1. **In-Process Package Tests (`internal/auth/devidm/server_test.go`)**:
   - Spins up `devidm.NewServer` on an `httptest.Server` and tests `auth.NewOIDCProvider` + `HandleLogin` + `HandleCallback` in-process.
2. **End-to-End Service Tests (`e2e/auth_e2e_test.go` & `.github/workflows/ci.yml`)**:
   - In GitHub Actions CI (`e2e` job), `./bin/dev-oidc` (`127.0.0.1:9000`) and `./bin/minicrm` (`127.0.0.1:8080`) are launched as separate OS processes alongside a `postgres:16-alpine` container.
   - `MINICRM_E2E_BASE_URL=http://127.0.0.1:8080 go test -v -race ./e2e/...` exercises the full browser redirect chain using an HTTP cookie jar, verifies `/threads` and `/settings` access, and queries PostgreSQL `audit_logs` to confirm the `user_login` audit trail entry was recorded.
   - When run locally without `MINICRM_E2E_BASE_URL` (`go test ./e2e/...`), the test automatically boots self-contained `devidm` and `minicrm` HTTP servers on ephemeral ports so `go test ./...` works with zero external setup.
