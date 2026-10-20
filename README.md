# MiniCRM

**MiniCRM** is a lightweight, self-contained email CRM built in Go. It connects to one or more shared IMAP/SMTP mailboxes (accounts), links incoming and outgoing emails into conversation threads, assigns threads to team members, and tracks them through a strict workflow (`new -> in-progress -> completed` or `new -> spam`).

Each mail account is monitored and replied from independently: an email delivered to several configured accounts produces one thread per account, and users only see threads, email bodies, and attachments of the accounts their OIDC groups grant them access to.

For detailed technical and security design documentation, see:
- [`docs/architecture.md`](./docs/architecture.md) — System overview, component diagram, technology stack, and package layout.
- [`docs/authentication.md`](./docs/authentication.md) — Production OAuth2/OIDC configuration, user provisioning flow, and local development with the dummy OIDC server (`cmd/dev-oidc`).
- [`docs/imap.md`](./docs/imap.md) — IMAP & SMTP server requirements, `INBOX` synchronization, `Archive`/`Junk`/`Sent` folder lifecycle, and safe deletion from `Archive`.
- [`docs/storage.md`](./docs/storage.md) — PostgreSQL schema, UTC `TIMESTAMPTZ` + `display_timezone`, immutable audit trail, structured query logging, and local disk attachment store.
- [`docs/workflow-and-threading.md`](./docs/workflow-and-threading.md) — Reply linking, completed-thread **Reopen & Merge**, workflow state machine, and pluggable assignment engine.
- [`docs/security.md`](./docs/security.md) — Defense-in-depth HTML sanitization, sandboxed `<iframe>` isolation, HTTP security headers, and CSRF protection.
- [`docs/testing.md`](./docs/testing.md) — Unit, integration, and end-to-end (`e2e/`) test setup, `prek` + `golangci-lint`, and GitHub Actions CI.

---

## Features

- **Multi-Account IMAP Ingestion & Folder Lifecycle**:
  - Any number of mail accounts can be configured (`MINICRM_ACCOUNT_<N>_*`); each account has its own IMAP/SMTP credentials, IMAP polling worker, and `Junk`/`Archive`/`Sent` folder lifecycle.
  - Pulls incoming email from each account's IMAP `INBOX` using basic `username + password` authentication.
  - Moves emails to the IMAP `Junk` folder when a thread is marked as `spam`.
  - Moves emails to the IMAP `Archive` folder when a thread is marked as `completed` (deleting emails from `Archive` on the mail server is safe and does not affect MiniCRM).
- **SMTP Outbound & `Sent` Sync**:
  - Sends replies through the SMTP account that the thread was received on, stores them in PostgreSQL, and `APPEND`s a copy to that account's IMAP `Sent` folder.
- **OIDC-Driven Account Access Control**:
  - Each account maps an OIDC group claim value (`MINICRM_ACCOUNT_<N>_OIDC_GROUP`) to the account; users only see threads, email bodies, and attachments of the accounts their groups grant.
  - Identical attachments delivered to multiple accounts are stored once on disk (content-addressed, SHA-256).
  - The UI shows an account badge/column and account filter for users with access to more than one account, and hides them for single-account users.
- **Reply Threading & Completed-Thread Reopen**:
  - Links replies via `In-Reply-To` and `References` headers, with a fallback to normalized subject + sender matching.
  - When a new reply arrives for an already `completed` thread, MiniCRM opens a new thread referencing the completed thread and offers a one-click **"Reopen & Merge"** action in the UI.
- **Assignment**:
  - Each account has its own configurable default assignee. When no default assignee is configured (or the configured one is inactive or lacks access), new threads stay **Unassigned** and are picked up automatically once a default assignee is set, or by an operator using **Take Thread**/assign, or by replying to the thread.
  - Neither the UI nor the backend allows assigning a thread to a user without access to the thread's mail account.
- **OAuth2 / OIDC Authentication & Audit Trail**:
  - Authenticates operators against an existing OAuth2/OIDC identity server (Authorization Code + PKCE) and supports pre-creating users before their first login.
  - Logs all user/auth/settings events, thread transitions, and data reads/mutations (`log/slog`).
- **Defense-in-Depth HTML Security**:
  - Sanitizes untrusted email HTML with `bluemonday` and renders email bodies inside isolated `<iframe sandbox>` frames with strict `Content-Security-Policy` and HTTP security headers. The iframe intentionally keeps the session cookie (no `credentialless`) so `/emails/{id}/body` can pass `RequireAuth` in Chromium-based browsers; the opaque sandbox origin still blocks access to parent cookies and DOM.

---

## Prerequisites

- **Go 1.27+**
- **PostgreSQL 16+**
- Access to an **IMAP/SMTP** mail server (username + password authentication)
- An **OAuth2 / OpenID Connect (OIDC)** identity provider

---

## Configuration

MiniCRM is configured via environment variables:

| Environment Variable | Required | Default | Description |
|---|---|---|---|
| `MINICRM_HTTP_ADDR` | No | `:8080` | HTTP listen address |
| `MINICRM_DATABASE_URL` | **Yes** | — | PostgreSQL connection URL |
| `MINICRM_ATTACHMENTS_DIR` | No | `./data/attachments` | Local directory for content-addressed attachments |
| `MINICRM_BOOTSTRAP_ADMIN_EMAIL` | **Yes** | — | Email of the initial admin user (granted access to all configured accounts) |
| `MINICRM_BOOTSTRAP_ADMIN_NAME` | No | `Admin` | Display name of the initial bootstrap admin |
| `MINICRM_DISPLAY_TIMEZONE` | No | `UTC` | Initial IANA display timezone (e.g. `Europe/Amsterdam`) |
| `MINICRM_MAIL_DOMAIN` | No | `minicrm.local` | Domain used when generating outbound `Message-ID` headers |
| `MINICRM_COOKIE_SECURE` | No | `true` | Set `Secure` attribute and `__Host-` prefix on session cookies |
| `MINICRM_OIDC_GROUPS_CLAIM` | No | `groups` | ID token claim carrying the user's group/role memberships |
| `MINICRM_OIDC_SCOPES` | No | `openid,profile,email` | Comma-separated scopes requested from the IdP; must include `openid`. Add `groups`/`groups_name` when the IdP (e.g. Kanidm) only emits groups on request |
| `MINICRM_ACCOUNT_<N>_ID` | **Yes** | — | Unique account identifier for account `N` (e.g. `support`). Accounts are numbered from `1` upwards until an `_ID` is absent |
| `MINICRM_ACCOUNT_<N>_NAME` | **Yes** | — | Human-readable account name shown in the UI |
| `MINICRM_ACCOUNT_<N>_OIDC_GROUP` | **Yes** | — | OIDC group/role claim value granting access to this account |
| `MINICRM_ACCOUNT_<N>_IMAP_HOST` | **Yes** | — | IMAP server hostname |
| `MINICRM_ACCOUNT_<N>_IMAP_PORT` | No | `993` | IMAP server port |
| `MINICRM_ACCOUNT_<N>_IMAP_USERNAME` | **Yes** | — | IMAP username |
| `MINICRM_ACCOUNT_<N>_IMAP_PASSWORD` | **Yes** | — | IMAP password |
| `MINICRM_ACCOUNT_<N>_IMAP_IMPLICIT_TLS` | No | `true` | Use implicit TLS (`true`) or STARTTLS (`false`) |
| `MINICRM_ACCOUNT_<N>_IMAP_POLL_INTERVAL` | No | `2m` | Polling interval for this account's `INBOX` synchronization |
| `MINICRM_ACCOUNT_<N>_IMAP_INITIAL_SYNC_DAYS` | No | `60` | On the first sync of a new account, only download mail received in the last N days. Set to `0` to backfill the complete mailbox history (large mailboxes can take a long time) |
| `MINICRM_ACCOUNT_<N>_SMTP_HOST` | **Yes** | — | SMTP server hostname |
| `MINICRM_ACCOUNT_<N>_SMTP_PORT` | No | `587` | SMTP server port |
| `MINICRM_ACCOUNT_<N>_SMTP_USERNAME` | **Yes** | — | SMTP username |
| `MINICRM_ACCOUNT_<N>_SMTP_PASSWORD` | **Yes** | — | SMTP password |
| `MINICRM_ACCOUNT_<N>_SMTP_FROM_ADDRESS` | **Yes** | — | Sender email address for outbound replies |
| `MINICRM_ACCOUNT_<N>_SMTP_FROM_NAME` | No | `MiniCRM Support` | Sender display name for outbound replies |
| `MINICRM_ACCOUNT_<N>_SMTP_TLS_MODE` | No | `starttls` | SMTP TLS mode: `starttls`, `implicit`, or `none` |
| `MINICRM_OIDC_ISSUER_URL` | **Yes** | — | OIDC issuer URL (must serve `.well-known/openid-configuration`). For Kanidm this is `https://<host>/oauth2/openid/<client>` |
| `MINICRM_OIDC_CLIENT_ID` | **Yes** | — | OAuth2 client ID |
| `MINICRM_OIDC_CLIENT_SECRET` | **Yes** | — | OAuth2 client secret |
| `MINICRM_OIDC_REDIRECT_URL` | **Yes** | — | OIDC callback URL (e.g. `https://crm.example.com/auth/callback`) |

### Example: Two Mail Accounts

```bash
export MINICRM_OIDC_GROUPS_CLAIM=groups

# Account 1: Support
export MINICRM_ACCOUNT_1_ID=support
export MINICRM_ACCOUNT_1_NAME="Customer Support"
export MINICRM_ACCOUNT_1_OIDC_GROUP=crm-support
export MINICRM_ACCOUNT_1_IMAP_HOST=imap.example.com
export MINICRM_ACCOUNT_1_IMAP_USERNAME=support@example.com
export MINICRM_ACCOUNT_1_IMAP_PASSWORD=...
export MINICRM_ACCOUNT_1_SMTP_HOST=smtp.example.com
export MINICRM_ACCOUNT_1_SMTP_USERNAME=support@example.com
export MINICRM_ACCOUNT_1_SMTP_PASSWORD=...
export MINICRM_ACCOUNT_1_SMTP_FROM_ADDRESS=support@example.com

# Account 2: Sales
export MINICRM_ACCOUNT_2_ID=sales
export MINICRM_ACCOUNT_2_NAME="Sales"
export MINICRM_ACCOUNT_2_OIDC_GROUP=crm-sales
export MINICRM_ACCOUNT_2_IMAP_HOST=imap.example.com
export MINICRM_ACCOUNT_2_IMAP_USERNAME=sales@example.com
export MINICRM_ACCOUNT_2_IMAP_PASSWORD=...
export MINICRM_ACCOUNT_2_SMTP_HOST=smtp.example.com
export MINICRM_ACCOUNT_2_SMTP_USERNAME=sales@example.com
export MINICRM_ACCOUNT_2_SMTP_PASSWORD=...
export MINICRM_ACCOUNT_2_SMTP_FROM_ADDRESS=sales@example.com
```

A user whose ID token contains `"groups": ["crm-support"]` may access the `support` account only; `["crm-support", "crm-sales"]` grants both. Users matching no configured account group are refused at login. Adding a third account later only requires `MINICRM_ACCOUNT_3_*` variables.

---

## Building & Running

```bash
# Build the single binary
go build -o minicrm ./cmd/minicrm

# Run MiniCRM (database migrations run automatically on startup)
./minicrm
```

### Docker Compose Test Stack

[`docker-compose.yml`](./docker-compose.yml) starts a complete, self-contained environment for exercising MiniCRM end-to-end:

| Service | Purpose | Host ports |
|---|---|---|
| `postgres` | PostgreSQL 16 database | `5432` |
| `mail` | Dovecot IMAP server with two seeded accounts | `1993` (IMAPS), `1143` (STARTTLS) |
| `smtp` | Mailpit catch-all SMTP server + web UI | `1025` (SMTP), `8025` (UI) |
| `dev-oidc` | Dummy OIDC identity provider (`cmd/dev-oidc`) | `9000` |
| `minicrm` | MiniCRM HTTP server and background workers | `8080` |
| `certgen` | One-shot generator for the self-signed IMAP/SMTP TLS certificate | — |

```bash
# Docker
docker compose up --build -d

# or Podman
podman-compose up --build -d
```

Open <http://localhost:8080/threads>. The dummy OIDC server signs you in automatically as **Dev Admin (admin@example.com)**, who has access to both mail accounts.

**Seeded mail accounts** (both use password `secret`):

| Account | Login | Seeded threads |
|---|---|---|
| Customer Support | `support@example.com` | Login issue with a reply, invoice question (HTML), feature request |
| Sales | `sales@example.com` | Pricing request with an attachment and a follow-up reply |

**Useful endpoints and notes:**

- Mailpit web UI: <http://localhost:8025> — inspect replies sent by MiniCRM through SMTP.
- Dovecot serves implicit TLS on `localhost:1993` and STARTTLS on `localhost:1143`. Inside the compose network MiniCRM uses `mail:993`; the published ports are only for debugging with a mail client.
- The OIDC issuer is `http://dev-oidc.localhost:9000`. Browsers resolve `*.localhost` to loopback, while the compose network aliases the name to the `dev-oidc` container, so the same issuer URL works for both the browser and MiniCRM.
- The self-signed CA is generated by the `certgen` service and trusted by the MiniCRM container at startup via `SSL_CERT_FILE`.
- State is stored in named volumes. Run `docker compose down -v` (or `podman-compose down -v`) to reset the database and re-seed the sample mail.
- All credentials in the compose file are development-only; never reuse them in production.

You can also point the live E2E test suite at the running stack:

```bash
MINICRM_E2E_BASE_URL=http://localhost:8080 \
MINICRM_DATABASE_URL='postgres://minicrm:minicrm@localhost:5432/minicrm?sslmode=disable' \
go test -v -race ./e2e/...
```

### Running Locally with the Dummy OIDC Server (`cmd/dev-oidc`)

For local development and testing without an external OIDC identity server, MiniCRM ships with a standalone dummy OIDC + PKCE server (`cmd/dev-oidc`) that automatically signs in with fixed credentials (`admin@example.com` by default):

```bash
# 1. Start the dummy OIDC server on 127.0.0.1:9000
go run ./cmd/dev-oidc \
  -addr 127.0.0.1:9000 \
  -email admin@example.com \
  -name "Dev Admin" \
  -groups "crm-support,crm-sales"

# 2. Point MiniCRM at the dummy OIDC server
export MINICRM_COOKIE_SECURE=false
export MINICRM_BOOTSTRAP_ADMIN_EMAIL=admin@example.com
export MINICRM_OIDC_ISSUER_URL=http://127.0.0.1:9000
export MINICRM_OIDC_CLIENT_ID=minicrm-dev
export MINICRM_OIDC_CLIENT_SECRET=minicrm-dev-secret
export MINICRM_OIDC_REDIRECT_URL=http://localhost:8080/auth/callback
go run ./cmd/minicrm
```

`cmd/dev-oidc` flags / environment variables:
- `-addr` / `DEV_OIDC_ADDR` (default `127.0.0.1:9000`)
- `-issuer` / `DEV_OIDC_ISSUER_URL` (default `http://<addr>`)
- `-client-id` / `DEV_OIDC_CLIENT_ID` (optional expected `client_id`)
- `-client-secret` / `DEV_OIDC_CLIENT_SECRET` (optional expected `client_secret`)
- `-sub` / `DEV_OIDC_SUB` (default `dev-admin-sub`)
- `-email` / `DEV_OIDC_EMAIL` (default `admin@example.com`)
- `-name` / `DEV_OIDC_NAME` (default `Dev Admin`)
- `-groups` / `DEV_OIDC_GROUPS` (comma-separated `groups` claim values, e.g. `crm-support,crm-sales`)

---

## Basic Usage

1. **Sign In (`/auth/login`)**:
   - Open `http://localhost:8080/threads` in your browser. You will be redirected to your OAuth2/OIDC identity provider to sign in.
2. **Working the Thread Inbox (`/threads`)**:
   - Filter threads by workflow state (`new`, `in_progress`, `completed`, `spam`) and, for users with access to multiple accounts, by mail account.
   - Click any thread to view its chronological messages, download attachments, or reassign it to another team member.
3. **Workflow Transitions & Replies (`/threads/{id}`)**:
   - **`new -> in_progress`**: Click **Start Progress** or send a reply using the **Send Reply** form (sending the first reply automatically transitions a `new` thread to `in_progress`).
   - **`new -> spam`**: Click **Mark as Spam (Move to Junk)** to mark the thread as spam and move its messages to the IMAP `Junk` folder.
   - **`in_progress -> completed`**: Click **Complete Thread (Move to Archive)** to complete the thread and move its messages from `INBOX` to the IMAP `Archive` folder.
   - **Reopening Completed Threads**: When a customer replies to a `completed` thread, a new thread is created with a banner linking to the completed thread. Click **Reopen completed thread & merge this email into existing thread** to reopen the original thread (`completed -> in_progress`) and merge the new message into it.
4. **Admin Settings & Pre-Creating Users (`/settings`)**:
   - Users with the `admin` role can visit `/settings` to update the **Default Assignee**, **Display Timezone (IANA)**, IMAP folder names (`INBOX`, `Archive`, `Junk`, `Sent`), and **Pre-Create Users** by email so they can be assigned threads before their first OIDC login.

---

## Development & Testing

```bash
# Run unit, integration, and self-contained E2E tests with race detector
go test -v -race ./...

# Run E2E tests against a live running MiniCRM + dev-oidc stack
MINICRM_E2E_BASE_URL=http://127.0.0.1:8080 go test -v -race ./e2e/...

# Run golangci-lint
golangci-lint run ./...

# Run local pre-commit checks via prek
prek run --all-files
```
