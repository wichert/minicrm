# Workflow State Machine, Reply Threading & Assignment

This document describes how MiniCRM links incoming and outgoing replies into conversation threads, handles replies to already completed threads, enforces the workflow state machine, and assigns threads to team members.

---

## 1. Reply Linking & Threading (`internal/threading/`)

Every email belongs to a `threads` record. When an incoming email is ingested, `threading.Matcher.ResolveThread` determines whether it belongs to an existing conversation or starts a new thread.

### Step 1: Subject Normalization (`NormalizeSubject`)
Leading reply and forward prefixes are stripped iteratively (case-insensitive):
- Supported prefixes: `Re:`, `Fw:`, `Fwd:`, `AW:`, `WG:`, `SV:`, `Antw:` (including numbered variants such as `Re[2]:`).
- Consecutive whitespace is collapsed into a single space.
- Example: `"AW: Re[2]:   Invoice #42 "` -> `"Invoice #42"`.

### Step 2: Candidate Thread Lookup
1. **Header Match (`In-Reply-To` & `References`) — Primary**:
   - Extracts normalized `Message-ID`s (without `<` and `>`) from `In-Reply-To` and `References` (ordered from most recent to oldest).
   - Queries `emails` for the most recent message matching any candidate `Message-ID` (`FindEmailByMessageIDs`).
   - If found, loads the parent email's `threads` record.
2. **Subject + Participant Match — Fallback**:
   - If no header match is found and the normalized subject is non-empty, MiniCRM queries `threads` (`FindThreadBySubjectAndParticipant`) where:
     - `lower(subject_normalized) = lower($norm_subject)`
     - `customer_email = $from_address`
     - `last_message_at >= $sent_at - ($subject_match_window_days || ' days')` (default `30` days, configurable in `/settings`)
     - `status != 'spam'`

### Step 3: Thread Resolution & Completed-Thread Handling
- **Case A: No matching thread (or matched thread is `spam`)**
  - Creates a new thread (`status = 'new'`) assigned via `assignment.Engine`.
  - Records `created` and `assigned` events in `thread_events`.
- **Case B: Matched thread is `new` or `in_progress`**
  - Links the incoming email (`parent_email_id = matched_email.id`) to the existing thread.
  - Advances `threads.last_message_at` and records `reply_received` in `thread_events`.
- **Case C: Matched thread is `completed` (`reopens_thread_id`)**
  - MiniCRM does **not** silently mutate a `completed` thread.
  - Instead, it creates a **new follow-up thread** (`status = 'new'`) with `reopens_thread_id` pointing to the matched `completed` thread.
  - When an operator views this new thread in `/threads/{id}`, a banner highlights the completed thread (`#ID`) and provides a one-click button:
    **"Reopen completed thread & merge this email into existing thread"**.

---

## 2. Workflow State Machine (`internal/workflow/service.go`)

```text
                  +---> [   spam    ]  (enqueues IMAP MOVE from INBOX to Junk)
                  |
[    new     ] ---+
                  |
                  +---> [ in_progress ] ---> [ completed ]  (enqueues IMAP MOVE from INBOX to Archive)
                               ^                   |
                               +-------------------+
                         (via explicit "Reopen & Merge" action)
```

### Transition Rules

| Current State | Target State | Trigger | Side Effects |
|---|---|---|---|
| `new` | `in_progress` | UI **Start Progress** button (`POST /threads/{id}/transition`) **or** sending the first outbound reply on a `new` thread | Updates `threads.status = 'in_progress'`; records `status_changed` in `thread_events`. |
| `new` | `spam` | UI **Mark as Spam** button (`POST /threads/{id}/transition`) | Updates `threads.status = 'spam'`; records `status_changed`; enqueues `move_to_junk` jobs in `imap_jobs` for all inbound emails in `INBOX`. |
| `in_progress` | `completed` | UI **Complete Thread** button (`POST /threads/{id}/transition`) | Updates `threads.status = 'completed'`; records `status_changed`; enqueues `move_to_archive` jobs in `imap_jobs` for all inbound emails in `INBOX`. |
| `completed` | `in_progress` | UI **Reopen & Merge** button (`POST /threads/{id}/reopen-merge`) | Moves all emails & events from the follow-up thread into the `completed` thread, transitions it to `in_progress`, logs `reopen_merged`, and deletes the temporary follow-up thread. |
| Any other | Any other | — | Rejected with `workflow.ErrInvalidTransition` (`HTTP 422`). |

---

## 3. Assignment Engine & Future Extensibility (`internal/assignment/`)

Every thread must be assigned to an active user (`threads.assignee_id NOT NULL`).

### Chain-of-Responsibility Pipeline
`assignment.Engine` evaluates an ordered slice of `assignment.Resolver` implementations:

```go
type Resolver interface {
    Name() string
    Resolve(ctx context.Context, input EmailInput) (*Candidate, error)
}
```

1. **V1 (`DefaultAssigneeResolver`)**:
   - Reads `account_settings.default_assignee_id` for the account the thread belongs to, verifies the user is active (`is_active = true`) **and** has access to that account, and returns `Candidate{AssigneeID: id, Source: "default", Confidence: 1.0}`.
   - If the account has no settings row, no default assignee, or the assignee is inactive/lacks account access, the resolver returns no candidate. The thread is then created **unassigned** (`assignee_id = NULL`, `assignment_source = 'unassigned'`).
2. **Unassigned Threads**:
   - Setting a default assignee in `/settings` calls `workflow.Service.AssignUnassignedThreadsForAccount`, which assigns all waiting `new`/`in_progress` threads of that account and logs an `assigned` event per thread.
   - Operators can **Take Thread** (assign to themselves), assign any user with access to the thread's account, or simply reply (which auto-assigns the thread to the replying operator).
3. **Manual Reassignment (`workflow.Service.Reassign`)**:
   - Operators can reassign any thread via `/threads/{id}/assign`.
   - The backend enforces that the target user is active **and** has access to the thread's account; otherwise `ErrAssigneeNoAccountAccess` is returned (`422 Unprocessable Entity`). The UI's assignee dropdown only lists users with access to the thread's account.
   - Sets `threads.assignment_source = 'manual'` and logs `assigned` (previously unassigned) or `reassigned` (with `old_value` and `new_value` JSON) in `thread_events`.
4. **Extending in V2 (Rules & ML Prediction)**:
   - Because V1 stores normalized subjects, sender addresses, body text, `assignment_source`, and all manual `reassigned` events in `thread_events`, future versions can prepend a `RuleBasedResolver` and `MLPredictorResolver` ahead of `DefaultAssigneeResolver` in `cmd/minicrm/main.go` without altering the database schema or ingestion worker.
