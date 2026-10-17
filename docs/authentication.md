# OAuth2 / OIDC Authentication & Dummy IDM (`dev-oidc`)

This document describes how **MiniCRM** authenticates users via OpenID Connect (OIDC), how users and pre-created assignees are provisioned and audited, and how to use the built-in dummy OIDC identity server (`cmd/dev-oidc`) for local development and automated end-to-end testing.

---

## 1. Production OAuth2 / OIDC Setup

MiniCRM uses the **OAuth2 Authorization Code Flow with PKCE (`S256`)** and **OpenID Connect (OIDC) Discovery** (`github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2`).

### Identity Provider (IdP) Requirements

Your OIDC identity server (e.g., Keycloak, Dex, Okta, Auth0, Entra ID, Authelia, Authentik) must:

1. Serve an OIDC Discovery document at `{MINICRM_OIDC_ISSUER_URL}/.well-known/openid-configuration`.
2. Publish its public signing keys at the `jwks_uri` advertised in the discovery document (`RS256` and `ES256` are both supported).
3. Support the `authorization_code` grant type and PKCE (`code_challenge_method=S256`).
4. Include the following claims in the signed `id_token` when scopes `openid profile email` are requested:
   - `sub` (unique subject identifier for the user)
   - `email` (email address, used to match pre-created users or bootstrap admin). Login fails if this claim is absent.
   - `name` (optional display name; falls back to `email` if omitted)
   - `nonce` (echoed from the `/authorize` request)
5. Include the group/role memberships claim named by `MINICRM_OIDC_GROUPS_CLAIM` (default `groups`) when the corresponding scope is requested. This claim decides which mail accounts a user may access (see [OIDC Group-Based Account Access](#oidc-group-based-account-access)).

> Some providers (e.g. Kanidm) only emit the groups claim when a dedicated scope is requested. Use `MINICRM_OIDC_SCOPES` to request it — see [Kanidm](#5-kanidm-setup).

### Registering MiniCRM as an OIDC Client

In your identity server, create a confidential OAuth2/OIDC client with:

- **Grant Type**: `authorization_code`
- **Response Type**: `code`
- **PKCE**: Required / `S256`
- **Redirect URI**: `https://<your-minicrm-domain>/auth/callback`
- **Scopes**: `openid`, `profile`, `email`, plus your groups scope (e.g. `groups` or `groups_name`)
- **Token signing**: `RS256` or `ES256` (MiniCRM verifies both via OIDC discovery / JWKS)

### MiniCRM OIDC Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `MINICRM_OIDC_ISSUER_URL` | **Yes** | — | Exact issuer URL of the OIDC provider (must match the `iss` claim in ID tokens and serve `/.well-known/openid-configuration`). |
| `MINICRM_OIDC_CLIENT_ID` | **Yes** | — | OAuth2 Client ID registered in the IdP. |
| `MINICRM_OIDC_CLIENT_SECRET` | **Yes** | — | OAuth2 Client Secret registered in the IdP. |
| `MINICRM_OIDC_REDIRECT_URL` | **Yes** | — | Full callback URL pointing to MiniCRM's `/auth/callback` endpoint. |
| `MINICRM_OIDC_GROUPS_CLAIM` | No | `groups` | ID token claim holding the user's group/role memberships; used to derive mail account access. |
| `MINICRM_OIDC_SCOPES` | No | `openid,profile,email` | Comma-separated scopes requested from the IdP. Must include `openid`. Add `groups`/`groups_name` (Kanidm) so group membership is returned. |
| `MINICRM_ACCOUNT_<N>_OIDC_GROUP` | **Yes** (per account) | — | Group/role claim value that grants access to mail account `N`. See [OIDC Group-Based Account Access](#oidc-group-based-account-access). |
| `MINICRM_BOOTSTRAP_ADMIN_EMAIL` | **Yes** | — | Email address of the initial admin user seeded on startup (granted access to all configured accounts). |
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
| `-groups` | `DEV_OIDC_GROUPS` | *(empty)* | Comma-separated fixed `groups` claim values, e.g. `crm-support,crm-sales`. |

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

---

## 5. OIDC Group-Based Account Access

MiniCRM supports multiple mail accounts (`MINICRM_ACCOUNT_<N>_*`). Each account declares the OIDC group/role value that grants access to it, so no separate MiniCRM permission database has to be maintained:

```env
MINICRM_OIDC_GROUPS_CLAIM=groups
MINICRM_OIDC_SCOPES=openid,profile,email,groups

MINICRM_ACCOUNT_1_ID=support
MINICRM_ACCOUNT_1_NAME="Customer Support"
MINICRM_ACCOUNT_1_OIDC_GROUP=crm-support

MINICRM_ACCOUNT_2_ID=sales
MINICRM_ACCOUNT_2_NAME="Sales"
MINICRM_ACCOUNT_2_OIDC_GROUP=crm-sales
```

**How the mapping works.** During the code exchange (`OIDCProvider.ExchangeAndVerify`), the ID token claim named by `MINICRM_OIDC_GROUPS_CLAIM` is read into `VerifiedIdentity.Groups` (a JSON array of strings, or a single string). On every login (`auth.Service.ResolveAndLoginUser`), each configured account whose `OIDC_GROUP` appears in that list is added to the user's `allowed_accounts`, which is persisted on `users` and refreshed on each login (changes are recorded in `audit_logs` as `user_updated`). Comparison is exact string matching, case-sensitive.

A user with `"groups": ["crm-support"]` can access `support` only; `["crm-support", "crm-sales"]` grants both. `OIDC_GROUP` values must match **exactly** what your provider puts in the claim — bare group names for some providers, `group@domain` SPNs or even UUIDs for others (see [Kanidm](#6-kanidm-setup) and [Other Providers](#7-other-providers)).

- Users matching **no** configured account group are refused at login with `403 Forbidden` and a `user_login_failed` audit event (`reason: "no_account_access"`).
- The bootstrap admin is granted access to all configured accounts.
- Admins can pre-create users and select their initial account access in `/settings`.
- `sqlc.User.HasAccountAccess(accountID)` and `sqlc.User.HasMultipleAccounts()` drive all thread, email body (`/emails/{id}/body`), and attachment (`/attachments/{id}/download`) authorization checks, as well as whether the account filter/column/badge is shown in the UI.

---

## 6. Kanidm Setup

[Kanidm](https://kanidm.github.io/kanidm/stable/) is a modern identity management platform with a standards-compliant OIDC provider. MiniCRM works with Kanidm, but there are four Kanidm-specific details to get right:

1. **The issuer URL is client-specific.** Kanidm uses per-client issuer URLs and signing keys:
   `https://<kanidm-host>/oauth2/openid/<client-name>`. This — not the site root — must be `MINICRM_OIDC_ISSUER_URL`.
2. **Kanidm signs ID tokens with `ES256` by default.** MiniCRM verifies it automatically from the discovery document; do **not** run `warning-enable-legacy-crypto` unless you have another reason to.
3. **Kanidm only returns the groups claim when a groups scope is requested.** Request `groups_name` (bare group names, e.g. `crm_support`) or `groups` (group SPNs *and* UUIDs, e.g. `crm_support@idm.example.com`). Set `MINICRM_OIDC_SCOPES` accordingly.
4. **Kanidm requires a scope map** covering every scope MiniCRM requests, including `openid`. Users must be members of a scope-mapped group or Kanidm rejects the authorization before MiniCRM is even reached.

### 6.1 Prerequisites

- A running Kanidm server, reachable over HTTPS by both browsers and the MiniCRM server.
- An administrative session (`kanidm login --name idm_admin`); members of `idm_admins` / `idm_oauth2_admins` can manage OAuth2 clients.
- A TLS certificate the MiniCRM binary trusts (Kanidm uses its own CA by default — install it in the host trust store, or use a publicly trusted certificate).

### 6.2 Create the groups and users

Create one Kanidm group per mail account, and one group for everyone who may sign in to MiniCRM at all (Kanidm requires a scope map for any user who authorizes the client):

```bash
kanidm group create crm_users
kanidm group create crm_support
kanidm group create crm_sales

# Users (repeat per operator); a mail address is required for the `email` claim.
kanidm person create alice "Alice Agent"
kanidm person update alice --mail alice@example.com
kanidm person credential create-reset-token alice   # let the user set a password/passkey

# Membership: crm_users = can sign in; crm_support / crm_sales = account access.
kanidm group add-members crm_users alice
kanidm group add-members crm_support alice
```

> Users without a `--mail` value get no `email` claim, and MiniCRM login fails with a 401 (`oidc id_token missing email claim`). Set the mail attribute for every operator.

### 6.3 Create the OAuth2 client

```bash
kanidm system oauth2 create minicrm "MiniCRM" https://crm.example.com
kanidm system oauth2 add-redirect-url minicrm https://crm.example.com/auth/callback

# Scope map: any member of crm_users may obtain these scopes.
# `groups_name` returns bare group names in the `groups` claim.
kanidm system oauth2 update-scope-map minicrm crm_users openid profile email groups_name
```

The scope map is an allow-list for *every* requested scope: if MiniCRM requests a scope that is missing here, the authorization is refused. Keep `MINICRM_OIDC_SCOPES` and this command in sync.

Optionally, grant a superset of scopes to a subset of users with `update-sup-scope-map`:

```bash
kanidm system oauth2 update-sup-scope-map minicrm crm_support groups_name
```

Retrieve the client secret:

```bash
kanidm system oauth2 show-basic-secret minicrm
```

Verify the effective configuration:

```bash
kanidm system oauth2 get minicrm
```

### 6.4 Configure MiniCRM

`MINICRM_ACCOUNT_<N>_OIDC_GROUP` must match the **exact strings Kanidm emits** in the claim:

- With the `groups_name` scope, the claim holds bare group names → use `crm_support`.
- With the `groups` scope, the claim holds SPNs and UUIDs → use `crm_support@idm.example.com`.

The safest choice is `groups_name`: the claim then contains the Kanidm group `name` attribute, which by convention uses lowercase letters, digits and `_` (e.g. `crm_support`).

```env
# Client-specific issuer URL — not the Kanidm site root.
MINICRM_OIDC_ISSUER_URL=https://idm.example.com/oauth2/openid/minicrm
MINICRM_OIDC_CLIENT_ID=minicrm
MINICRM_OIDC_CLIENT_SECRET=<output of show-basic-secret>
MINICRM_OIDC_REDIRECT_URL=https://crm.example.com/auth/callback

# Kanidm needs the groups_name scope to emit group membership.
MINICRM_OIDC_GROUPS_CLAIM=groups
MINICRM_OIDC_SCOPES=openid,profile,email,groups_name

MINICRM_BOOTSTRAP_ADMIN_EMAIL=alice@example.com
MINICRM_BOOTSTRAP_ADMIN_NAME="Alice Agent"

MINICRM_ACCOUNT_1_ID=support
MINICRM_ACCOUNT_1_NAME="Customer Support"
MINICRM_ACCOUNT_1_OIDC_GROUP=crm_support
# ... IMAP/SMTP settings for account 1 ...

MINICRM_ACCOUNT_2_ID=sales
MINICRM_ACCOUNT_2_NAME="Sales"
MINICRM_ACCOUNT_2_OIDC_GROUP=crm_sales
# ... IMAP/SMTP settings for account 2 ...
```

`MINICRM_BOOTSTRAP_ADMIN_EMAIL` must be the address of a Kanidm person that also has the `mail` attribute set, otherwise the bootstrap admin is created but can never log in.

### 6.5 Using a custom claim instead of `groups`

If you prefer clean, account-oriented values (and to avoid coupling MiniCRM configuration to Kanidm group names), map a dedicated claim:

```bash
kanidm system oauth2 update-claim-map minicrm minicrm_accounts crm_support support
kanidm system oauth2 update-claim-map minicrm minicrm_accounts crm_sales sales
kanidm system oauth2 update-claim-map-join minicrm minicrm_accounts array
```

Then:

```env
MINICRM_OIDC_GROUPS_CLAIM=minicrm_accounts
MINICRM_ACCOUNT_1_OIDC_GROUP=support
MINICRM_ACCOUNT_2_OIDC_GROUP=sales
```

Each group may contribute one or more values, and values from several groups are merged into the array. Do **not** use `csv` or `ssv` joins — MiniCRM expects a JSON array (or a single string), and `array` is Kanidm's default.

> **Verify this first.** Custom claims are only emitted when the client is granted a groups-related scope, so keep the `groups_name` (or `groups`) scope in both the scope map and `MINICRM_OIDC_SCOPES` while using a claim map. Log in once and confirm the resolved access in PostgreSQL (`SELECT email, allowed_accounts FROM users;`); an empty array means the claim was not present, in which case fall back to the `groups` claim (section 6.4).

### 6.6 Verify the setup

1. Inspect the discovery document to confirm the issuer matches your configuration:
   ```bash
   curl -s https://idm.example.com/oauth2/openid/minicrm/.well-known/openid-configuration | jq '{issuer, authorization_endpoint, token_endpoint, jwks_uri, id_token_signing_alg_values_supported, code_challenge_methods_supported}'
   ```
   `issuer` must equal `MINICRM_OIDC_ISSUER_URL` exactly (no trailing slash).
2. Start MiniCRM and open `/threads`. You are redirected to Kanidm, which shows a consent prompt for the requested scopes.
3. After signing in, MiniCRM shows only the threads of the accounts your Kanidm groups grant. A user in `crm_support` only must not see `sales` threads; the account filter/column is hidden for them.
4. Confirm access was recorded: the `audit_logs` table contains `user_login` and (when groups changed) `user_updated` events, and `users.allowed_accounts` lists the resolved account IDs:
   ```sql
   SELECT email, allowed_accounts FROM users ORDER BY email;
   ```

### 6.7 Troubleshooting

| Symptom | Cause & fix |
|---|---|
| `403 Forbidden` at `/auth/callback`, audit event `user_login_failed` with `reason: "no_account_access"` | The groups claim is missing or the values do not match. Check `MINICRM_OIDC_SCOPES` includes `groups_name` (or `groups`), that `update-scope-map` grants that scope, and that `MINICRM_ACCOUNT_<N>_OIDC_GROUP` matches the emitted string exactly (bare name vs `group@domain`). |
| `401 Unauthorized` at callback, log `oidc id_token missing email claim` | The Kanidm person has no `mail` attribute (`kanidm person update <user> --mail ...`). |
| Kanidm shows "Invalid request" / scope error | MiniCRM requested a scope not present in a scope map. Align `MINICRM_OIDC_SCOPES` with `update-scope-map minicrm <group> <scopes...>`. |
| `failed to discover oidc provider` on startup | `MINICRM_OIDC_ISSUER_URL` points at the Kanidm root instead of `https://<host>/oauth2/openid/<client>`, or the host does not trust Kanidm's TLS CA. |
| `x509: certificate signed by unknown authority` | Install Kanidm's CA into the MiniCRM host trust store (or use a publicly trusted certificate). |
| Login loops back to Kanidm, then `invalid oauth2 state` | The redirect URI configured in Kanidm (`add-redirect-url`) differs from `MINICRM_OIDC_REDIRECT_URL` (scheme/host/port/path must match exactly), so the session cookie is not returned. |
| Changes to group membership have no effect | `allowed_accounts` is refreshed at login only. Have the user sign out and in again; effective scopes may also be cached in the existing Kanidm session. |
| Users are prompted for consent every time scopes change | Expected OIDC behaviour. In a governed enterprise deployment you may disable the prompt with `kanidm system oauth2 disable-consent-prompt minicrm`. |

---

## 7. Other Providers

Anything that supports OIDC Discovery, `authorization_code`, PKCE `S256`, and either `RS256` or `ES256` works without MiniCRM-specific code. Notable configuration points:

| Provider | Issuer URL | Groups claim | Notes |
|---|---|---|---|
| Keycloak | `https://<host>/realms/<realm>` | `groups` (needs a group-membership mapper) | Add a "Group Membership" client scope/mapper; disable the full group path if you want bare names. |
| Dex | `https://<host>/dex` | `groups` | Enable the `groups` claim in the static client config. |
| Authentik | `https://<host>/application/o/<slug>/` | `groups` | Add a scope mapping that emits `groups`. |
| Entra ID | `https://login.microsoftonline.com/<tenant>/v2.0` | `groups` | Returns **group object IDs**, not names — set `MINICRM_ACCOUNT_<N>_OIDC_GROUP` to the GUID, or use app roles with `roles`. |
| Kanidm | `https://<host>/oauth2/openid/<client>` | `groups` / `groups_name` | See [Kanidm Setup](#6-kanidm-setup); `ES256` and a per-client issuer. |
