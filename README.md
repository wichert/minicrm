# MiniCRM

**MiniCRM** is a lightweight, self-contained email CRM built in Go. It connects to a shared IMAP/SMTP mailbox, links incoming and outgoing emails into conversation threads, assigns every thread to a team member, and tracks threads through a strict workflow (`new -> in-progress -> completed` or `new -> spam`).

For detailed technical and security design documentation, see [`docs/architecture.md`](./docs/architecture.md).

---

## Features

- **IMAP Ingestion & Folder Lifecycle**:
  - Pulls incoming email from an IMAP server (`INBOX`) using basic `username + password` authentication.
  - Moves emails to the IMAP `Junk` folder when a thread is marked as `spam`.
  - Moves emails to the IMAP `Archive` folder when a thread is marked as `completed` (deleting emails from `Archive` on the mail server is safe and does not affect MiniCRM).
- **SMTP Outbound & `Sent` Sync**:
  - Sends replies via SMTP (`username + password` authentication), stores them in PostgreSQL, and `APPEND`s a copy to the IMAP `Sent` folder.
- **Reply Threading & Completed-Thread Reopen**:
  - Links replies via `In-Reply-To` and `References` headers, with a fallback to normalized subject + sender matching.
  - When a new reply arrives for an already `completed` thread, MiniCRM opens a new thread referencing the completed thread and offers a one-click **"Reopen & Merge"** action in the UI.
- **Mandatory Assignment**:
  - Every thread is assigned to a user, with a configurable default assignee and a pluggable resolver pipeline ready for future rule-based or ML assignment.
- **OAuth2 / OIDC Authentication & Audit Trail**:
  - Authenticates operators against an existing OAuth2/OIDC identity server (Authorization Code + PKCE) and supports pre-creating users before their first login.
  - Logs all user/auth/settings events, thread transitions, and data reads/mutations (`log/slog`).
- **Defense-in-Depth HTML Security**:
  - Sanitizes untrusted email HTML with `bluemonday` and renders email bodies inside isolated, `credentialless` `<iframe sandbox>` frames with strict `Content-Security-Policy` and HTTP security headers.

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
| `MINICRM_BOOTSTRAP_ADMIN_EMAIL` | **Yes** | — | Email of the initial admin user & default assignee |
| `MINICRM_BOOTSTRAP_ADMIN_NAME` | No | `Admin` | Display name of the initial bootstrap admin |
| `MINICRM_DISPLAY_TIMEZONE` | No | `UTC` | Initial IANA display timezone (e.g. `Europe/Amsterdam`) |
| `MINICRM_MAIL_DOMAIN` | No | `minicrm.local` | Domain used when generating outbound `Message-ID` headers |
| `MINICRM_COOKIE_SECURE` | No | `true` | Set `Secure` attribute and `__Host-` prefix on session cookies |
| `MINICRM_IMAP_HOST` | **Yes** | — | IMAP server hostname |
| `MINICRM_IMAP_PORT` | No | `993` | IMAP server port |
| `MINICRM_IMAP_USERNAME` | **Yes** | — | IMAP username |
| `MINICRM_IMAP_PASSWORD` | **Yes** | — | IMAP password |
| `MINICRM_IMAP_IMPLICIT_TLS` | No | `true` | Use implicit TLS (`true`) or STARTTLS (`false`) |
| `MINICRM_IMAP_POLL_INTERVAL` | No | `2m` | Polling interval for `INBOX` synchronization |
| `MINICRM_SMTP_HOST` | **Yes** | — | SMTP server hostname |
| `MINICRM_SMTP_PORT` | No | `587` | SMTP server port |
| `MINICRM_SMTP_USERNAME` | **Yes** | — | SMTP username |
| `MINICRM_SMTP_PASSWORD` | **Yes** | — | SMTP password |
| `MINICRM_SMTP_FROM_ADDRESS` | **Yes** | — | Sender email address for outbound replies |
| `MINICRM_SMTP_FROM_NAME` | No | `MiniCRM Support` | Sender display name for outbound replies |
| `MINICRM_SMTP_TLS_MODE` | No | `starttls` | SMTP TLS mode: `starttls`, `implicit`, or `none` |
| `MINICRM_OIDC_ISSUER_URL` | **Yes** | — | OIDC issuer URL (must serve `.well-known/openid-configuration`) |
| `MINICRM_OIDC_CLIENT_ID` | **Yes** | — | OAuth2 client ID |
| `MINICRM_OIDC_CLIENT_SECRET` | **Yes** | — | OAuth2 client secret |
| `MINICRM_OIDC_REDIRECT_URL` | **Yes** | — | OIDC callback URL (e.g. `https://crm.example.com/auth/callback`) |

---

## Building & Running

```bash
# Build the single binary
go build -o minicrm ./cmd/minicrm

# Run MiniCRM (database migrations run automatically on startup)
./minicrm
```

---

## Basic Usage

1. **Sign In (`/auth/login`)**:
   - Open `http://localhost:8080/threads` in your browser. You will be redirected to your OAuth2/OIDC identity provider to sign in.
2. **Working the Thread Inbox (`/threads`)**:
   - Filter threads by workflow state (`new`, `in_progress`, `completed`, `spam`).
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
# Run unit and integration tests with race detector
go test -v -race ./...

# Run golangci-lint
golangci-lint run ./...

# Run local pre-commit checks via prek
prek run --all-files
```
