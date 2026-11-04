# Multi-User Coordination

Two people open the same record, one claims a ticket while another is
assigning it, a viewer's tab drops off the network halfway through an edit.
Tavern does not decide any of these cases. It tells viewers that something
changed and reports honestly when it could not. Who may write, whose save
wins and who owns a record are decisions your application makes in its own
storage, at the moment of each write.

This guide shows how to build those decisions on top of Tavern. For the
delivery patterns it relies on, see the
[Subscription Guide](subscription-guide.md) and the
[Lifeline + Scoped Stream Contract](stream-contract.md). Short copyable
versions are in the cookbook:
[Recipe 33](../RECIPES.md#33-versioned-save-with-conflict-recovery) and
[Recipe 34](../RECIPES.md#34-claim-a-record-with-an-expiring-lease).

Go examples use `database/sql` and Postgres-style SQL. Functions such as
`currentUser`, `canEdit`, `render` and `recordTopic` stand for your own
session, permission, template and naming code; they are not Tavern APIs.

---

## Who owns which state

| State | Lives in | Who sees it | Authoritative? | Examples |
|---|---|---|---|---|
| **Local interaction** | The browser tab | That tab only | No | Unsaved draft, focus, selection, open menu, scroll position |
| **Advisory presence** | Process memory or a shared store, with expiry | Viewers of the same context | No, a hint | "Bob is viewing", "Alice is editing the title" |
| **Application state** | Your database | Anyone authorized | Yes | Record contents and version, assignee, claim holder and expiry |

Keep each kind where it belongs:

- Local interaction never goes over SSE. Do not publish drafts, keystrokes or
  cursor positions to shared topics; they leak unsaved (and possibly
  sensitive) text and flood every viewer's queue.
- Presence can be wrong for a while and nothing breaks. Use it to warn
  people, never to decide a write.
- Application state changes only through server handlers that check
  identity, permission and the expected version in the same atomic write.

## What delivery signals do not tell you

Tavern reports delivery facts: a subscription exists, a stream is open, a
replay filled a gap or could not, a queue dropped a message. None of these
is a fact about your records.

| Signal | Means | Does not mean |
|---|---|---|
| Stream open, `sseOpen`, `tavern-reconnected` | A connection exists and replay ran | The user may still edit, still holds a claim, or has seen the latest version |
| `HasSubscribers`, `SubscriberCount` | Someone in this process is subscribed | Someone is editing, or the record is "taken" |
| Stream ended, presence entry gone | The connection or its heartbeats stopped | The user gave up their claim, or their draft is gone |
| Message queued or replayed | Tavern queued it for the subscriber | The browser applied it, or the state it describes is still current |
| Scope (`SubWithScope`, `PublishTo`) | Delivery is restricted to that scope | The subscriber is allowed to see it; derive the scope from the session first |

A server that grants permission or ownership because a connection is open
breaks the first time a laptop sleeps or two tabs share one user. Store
ownership in the database and check it on every write.

---

## Arbitrated writes

The server decides every conflicting write. The usual tool is optimistic
concurrency: each record carries a version, every edit says which version it
started from, and the database applies the edit only if that is still the
current version.

### Two users edit the same record

1. Alice and Bob both open record 7 at version 8. Each form carries
   `version=8`.
2. Bob saves first. The server checks his session and permission, then runs
   one conditional update: set the new contents and `version = 9` where
   `version = 8`. One row changes, the transaction commits, and the server
   publishes "record 7 is now version 9".
3. Alice's tab receives that invalidation. Her form is focused, so the page
   shows "Bob saved a newer version" next to it and leaves her draft alone
   (see [Recovery without losing drafts](#recovery-without-losing-drafts)).
4. Alice saves anyway with `version=8`. The same conditional update now
   matches no row. Nothing is written and nothing is published. The server
   answers with a conflict: Bob's version 9 alongside Alice's draft.
5. Alice chooses: reload version 9 and drop her draft, resubmit her draft as
   a deliberate edit of version 9, or merge by hand. Whatever she submits
   next is checked against version 9 the same way.

Neither save overwrites the other without someone seeing both. The
comparison and the write happen in one statement, so two concurrent saves
from version 8 cannot both succeed. In Postgres at the default
`READ COMMITTED` level, the second `UPDATE` waits for the first to commit,
then rechecks its `WHERE` clause against the new row and matches nothing;
at `SERIALIZABLE` it fails with a serialization error that you retry.
Reading the version first and updating in a second statement
(check-then-act) does not give that guarantee unless the read locks the row
(`SELECT ... FOR UPDATE`) for the rest of the transaction.

```go
var ErrStale = errors.New("record changed since it was loaded")

// Save writes the record only if it is still at expected. It returns the
// new version, or ErrStale if no row matched.
func (s *Store) Save(ctx context.Context, tenant, id string, expected int64, body string) (int64, error) {
	var version int64
	err := s.db.QueryRowContext(ctx, `
		UPDATE records SET body = $1, version = version + 1
		WHERE tenant_id = $2 AND id = $3 AND version = $4
		RETURNING version`,
		body, tenant, id, expected).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrStale // stale, or the record is gone: Load tells which
	}
	return version, err
}
```

The handler derives identity from the session, checks permission, and
publishes only after the update has committed:

```go
func saveRecord(store *Store, broker *tavern.SSEBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := currentUser(r) // from the session, never from the form
		id := r.PathValue("id")
		if !canEdit(r.Context(), user, id) {
			http.Error(w, "You cannot edit this record", http.StatusForbidden)
			return
		}
		expected, err := strconv.ParseInt(r.FormValue("version"), 10, 64)
		if err != nil {
			http.Error(w, "Missing record version", http.StatusBadRequest)
			return
		}
		draft := r.FormValue("body")

		version, err := store.Save(r.Context(), user.TenantID, id, expected, draft)
		switch {
		case errors.Is(err, ErrStale):
			current, err := store.Load(r.Context(), user.TenantID, id)
			if errors.Is(err, ErrNotFound) {
				http.Error(w, "This record was deleted", http.StatusNotFound)
				return
			}
			if err != nil {
				http.Error(w, "Could not load the record", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusConflict)
			render(w, "conflict", conflictView{Current: current, Draft: draft})
			return
		case err != nil:
			http.Error(w, "Could not save the record", http.StatusInternalServerError)
			return
		}

		broker.Publish(recordTopic(user.TenantID, id),
			tavern.NewSSEMessage("record-changed", strconv.FormatInt(version, 10)).String())
		render(w, "saved", savedView{ID: id, Version: version})
	}
}
```

### 409 or 412

The example uses a form field and `409 Conflict`, because an HTML form cannot
send conditional headers and the conflict is an application state the user
resolves. An HTTP API can use the standard precondition headers instead: send
the version as an `ETag`, have clients send it back in `If-Match`, and answer
`412 Precondition Failed` when it no longer matches
([RFC 9110 §13.1.1](https://www.rfc-editor.org/rfc/rfc9110#section-13.1.1),
[§15.5.13](https://www.rfc-editor.org/rfc/rfc9110#section-15.5.13)). If
updates must be conditional, answer requests without `If-Match` with
`428 Precondition Required`
([RFC 6585 §3](https://www.rfc-editor.org/rfc/rfc6585#section-3)). Either
way, compare the version inside the write, not in a separate read.

htmx does not swap 4xx responses by default;
[Recipe 33](../RECIPES.md#33-versioned-save-with-conflict-recovery) shows the
`responseHandling` setting that lets the conflict fragment swap.

The conflict fragment keeps the user's draft in the form, shows the current
saved version beside it, and sets the hidden `version` to the current one so
that resubmitting is a deliberate edit of what the user has now seen.

### Conflicts, advisory hints and claims are different tools

| Tool | Prevents | Cost | Use when |
|---|---|---|---|
| Versioned writes | Silent overwrites | A rejected save the user must resolve | Always, for shared mutable records |
| Editing hints (presence) | Nothing; warns people early | Low-frequency publishes | People often edit the same record at once |
| Claims and leases | Concurrent work on a record | Claims to renew, expire and take over | Work must be exclusive, such as picking up a ticket |

Hints and claims reduce conflicts; they do not replace versioned writes.
Keep the version check even on claimed records, because an admin action or
an expired claim can still change a record under the holder.

---

## Commit, then publish

Publish an invalidation only after the change it describes has committed.

- **Arbitrate and commit first.** If the transaction rolls back, publish
  nothing: viewers would refetch and find no change, or worse, render state
  that never existed.
- **Publish after commit.** Calling `Publish` inside the transaction tells
  viewers about data they cannot read yet, and it cannot be taken back.
- **Expect a gap between the two.** If the process stops after the commit
  and before `Publish`, the change is saved but nobody is told. Tavern has no
  transaction spanning your database and its broker.

Close that gap the way the rest of the system already recovers:

- **Versions.** Viewers refetch canonical state when their stream (re)opens,
  and every response carries the current version. A missed invalidation is
  corrected by the next one, the next reconnect, or the next save attempt,
  which fails with a conflict instead of overwriting.
- **Periodic reconciliation.** Long-lived pages refetch on a timer, or
  compare versions in a lightweight status request.
- **An outbox.** To make sure every commit leads to a publish attempt,
  write an outbox row in the same transaction as the change, and have a
  worker publish rows after commit and then delete them. A crash between
  the publish and the delete repeats the publish, so include the version
  and let clients ignore versions they already have.

An outbox guarantees attempts, not delivery. `Publish` returns nothing: a
full subscriber queue drops the message, and with a backend, a failed or
unhealthy backend is logged or skipped rather than reported to the caller.
Viewers still need the version checks and reconciliation above. If another
system must confirm that it received each change, use a durable transport
with acknowledgements for that path; Tavern does not provide one.

`Publish` reaches subscribers in this process. With several instances,
configure a backend
([Recipe 25](../RECIPES.md#25-distributed-fan-out----multi-instance-deployment))
so the instance that committed can tell viewers connected elsewhere.

---

## Claims, leases and assignment

A claim says "this person is working on this record; others should not".
Model it as columns on the record, change it only with conditional updates,
and check it on every write that depends on it.

```sql
-- Illustrative columns; adapt to your schema.
ALTER TABLE records
  ADD COLUMN assignee      text,                       -- persistent ownership
  ADD COLUMN claim_holder  text,                       -- current lease holder
  ADD COLUMN claim_token   bigint NOT NULL DEFAULT 0,  -- bumped on every new claim
  ADD COLUMN claim_expires timestamptz;
```

- **Assignment** is persistent: the ticket stays Alice's until someone
  reassigns it, whether or not she is online.
- **A lease** expires: "Alice is working on this" lasts as long as she keeps
  renewing it. When it expires anyone may take it.
- **The token** identifies one particular claim. Each new claim increments
  it, so a holder whose lease expired and was taken over cannot keep writing
  with their old claim, even if they reclaim it later.

### Checking a lease

Whether a lease is live is decided once per operation, by the database,
while the operation holds the record's row lock. Every claim operation runs
in a short transaction:

1. Lock the row: `SELECT 1 FROM records WHERE tenant_id = $1 AND id = $2 FOR UPDATE`.
   Waiting for another operation on the record happens here.
2. Run the conditional update, comparing expiry with `clock_timestamp()`.
3. Commit. Never keep this transaction open longer than one request.

```go
// withRecordLock runs fn in a short transaction that holds the record's
// row lock. Lease checks in fn see the current clock and the latest row.
func (s *Store) withRecordLock(ctx context.Context, tenant, id string, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
		return err
	}
	var locked int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM records WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenant, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
```

Why the extra step:

- In Postgres, `now()` is the time the transaction started, and
  `statement_timestamp()` is when the server received the statement
  ([Postgres: Current Date/Time](https://www.postgresql.org/docs/current/functions-datetime.html#FUNCTIONS-DATETIME-CURRENT)).
  An update that checks `claim_expires > now()` compares with the start of
  its transaction, and one that checks `statement_timestamp()` and then
  waits for a row lock compares with the time before the wait. Either can
  renew or write with a lease that has already expired.
- Once the transaction holds the row lock, nothing else can change the row,
  and `clock_timestamp()` reads the clock when the update runs. Version and
  token comparisons need no lock: after waiting, Postgres rechecks them
  against the latest row
  ([Postgres: Read Committed](https://www.postgresql.org/docs/current/transaction-iso.html#XACT-READ-COMMITTED)).
- Every operation that can take over or change a claim also locks the row
  first, so it runs entirely before or entirely after a holder's write. A
  holder's write that saw a live lease may commit a moment after the lease
  runs out, but always before anyone else can claim the record.
- `lock_timeout` makes a request that cannot get the lock fail rather than
  wait indefinitely.

The SQL assumes Postgres at the default `READ COMMITTED` level, one primary
database, and a lease long compared with the renewal interval and a
transaction. Under `SERIALIZABLE`, retry on serialization errors.

| Operation | Update, run after locking the row | No row matched |
|---|---|---|
| **Acquire** | `SET claim_holder = $user, claim_token = claim_token + 1, claim_expires = clock_timestamp() + interval '2 minutes' WHERE ... AND (claim_holder IS NULL OR claim_expires <= clock_timestamp()) RETURNING claim_token` | Someone else holds a live claim |
| **Renew** | `SET claim_expires = clock_timestamp() + interval '2 minutes' WHERE ... AND claim_holder = $user AND claim_token = $token AND claim_expires > clock_timestamp()` | The claim expired or was taken over; tell the user |
| **Release** | `SET claim_holder = NULL, claim_expires = NULL WHERE ... AND claim_holder = $user AND claim_token = $token` | Someone else has claimed it since; an expired claim of your own still matches and is cleared |
| **Write under a claim** | `SET ..., version = version + 1 WHERE ... AND version = $expected AND claim_holder = $user AND claim_token = $token AND claim_expires > clock_timestamp()` | Stale version or lost claim; load to tell which |
| **Assign** | `SET assignee = $to, claim_holder = NULL, claim_expires = NULL, claim_token = claim_token + 1, version = version + 1 WHERE ... AND version = $expected` | Stale; show the current assignment |

`WHERE ...` stands for `tenant_id = $tenant AND id = $id`.

Two people who press "Claim" at once both wait for the row lock. The first
sets a holder; the second then finds a live claim and matches no row. The
losing request should answer with who holds the claim now, read after the
failed update, for display only.

Policy stays with the application. Whether a second tab of the same user
takes over the claim, how long a lease lasts, who may break someone else's
claim and whether assignment implies a claim are product decisions; the
updates above are one consistent set of answers.

Things that do not establish or end a claim:

- **A connection or a heartbeat.** A dropped connection may be a train
  tunnel, and a reconnect does not renew anything. Renew with an explicit
  request (a timer while the edit form is open, or each save) that goes
  through the renew update.
- **Presence.** A presence entry expiring is not the user releasing their
  claim. Let the lease expire on its own clock, or let an authorized user
  break it explicitly.
- **Process memory.** A `sync.Mutex` or an in-memory map of holders protects
  one process. It stops meaning anything when a second instance starts or
  this one restarts.
- **A held database lock.** Do not keep a transaction or row lock open for
  the life of an SSE connection or an edit session. Leases exist so that
  ownership survives without holding a lock.

After each claim change, publish an invalidation for the record so viewers
refetch who holds it, following [Commit, then publish](#commit-then-publish).

---

## Presence and editing hints

Presence answers "who is here?" and, at most, "who is editing what?". It is
for display only.

The [`presence`](../presence/presence.go) package tracks one entry per user
per topic in the current process, expires entries that stop heartbeating,
and can publish a rendered list to `<topic>:presence`
([Recipe 24](../RECIPES.md#24-presence-tracking----whos-viewing-a-document)).
An editing hint fits in an entry's metadata:

```go
// On focus of the edit form, and again with "" on blur or save.
// Every Update publishes the whole list, so never call this per keystroke.
tracker.Update(recordTopic(user.TenantID, id), user.ID, map[string]any{"editing": "title"})
```

Know what it does and does not do:

- **One entry per user.** `Join` for a user already present is a no-op, and
  `Leave` removes the user's entry. If one user has two tabs open, closing
  either removes them. Count connections per user in your handler and call
  `Leave` when the last one closes, or use a per-tab ID as `UserID` and
  group by user when rendering.
- **One process.** Each instance has its own tracker and publishes the list
  it knows. With a backend fanning out across instances, those partial lists
  overwrite each other. For presence across instances, keep entries with an
  expiry in shared storage and render from there.
- **Expiry is approximate.** An entry disappears after `StaleTimeout`
  without heartbeats; a vanished user may come back seconds later. That is
  fine for a hint and wrong for ownership.

---

## Placing coordination on streams

The [stream contract](stream-contract.md) splits a page into a long-lived
lifeline and short-lived scoped streams.

- **Lifeline:** low-frequency notices that matter wherever the user is, such
  as "a ticket was assigned to you" or a presence count.
- **Scoped stream:** the open record's invalidations, claim changes and
  editing hints. It opens with the record panel, closes with it, and is
  authorized for that record when it opens.

"Scoped stream" is a connection that follows one view. It is a separate
idea from broker scoping (`SubWithScope` / `PublishTo`), which restricts a
shared topic to one scope value. Either way, check access before
subscribing; a scope selects what is delivered, it does not grant access to
it. See [Authorization](production-operations.md#authorization).

---

## Recovery without losing drafts

After loss, a gap or a reconnect, refetch canonical state, but never
replace what the user is typing.

- **Refetch, do not infer.** A replay or `tavern-reconnected` says what was
  delivered, not what is current. Fetch the record, its version and its
  claim from the server.
- **Subscribe, then fetch.** Fetching and then subscribing misses changes in
  between. Send a frame once the subscription exists and fetch when it
  arrives, or compare versions; see
  [Distinct invalidations](subscription-guide.md#distinct-invalidations).
- **Leave a focused form alone.** Do not make the edit form an SSE swap
  target. Let invalidations refresh a separate notice beside it, and let the
  server decide what the notice says by comparing the version the form
  started from with the current one.

```html
<form id="record-form" hx-post="/records/7" hx-swap="outerHTML">
  <input type="hidden" name="version" value="8">
  <textarea name="body">…</textarea>
  <button>Save</button>
</form>

<!-- Refreshed on every (re)connect and every change; the form is not. -->
<div id="record-notice"
     hx-get="/records/7/notice"
     hx-trigger="sse:ready, sse:record-changed"
     hx-include="#record-form [name=version]"></div>
```

The notice handler renders nothing when the form's version is current, and
otherwise "Bob saved version 9 at 10:42" with a reload link. It can also say
"Your claim on this record expired" by checking the claim columns, so a
reconnect never implies that a claim survived. The
[task board example](../examples/taskboard/) uses the same `ready` frame and
`sse:` triggers for its columns.

---

## Failure scenarios

| What happens | Result | Handled by |
|---|---|---|
| Two users save from the same version | One commits; the other gets 409 and keeps their draft | Conditional update |
| Two users claim at once | One holds the claim; the other sees who does | Conditional acquire |
| A claim expires and someone else takes it | The old holder's renew and writes match no row | Token and expiry in every write |
| A record is reassigned during an edit | The old holder's save fails; the notice shows the new assignee | Token bump and version check |
| A transaction rolls back | No invalidation is sent | Publish after commit |
| The process stops between commit and publish | Viewers are stale until their next refetch | Versions, reconciliation or an outbox |
| A viewer's queue overflows | The stream ends, the browser reconnects and refetches | `SubWithDisconnectOnLoss` and the ready frame |
| A tab sleeps and reconnects | Drafts stay; the notice reports newer versions or a lost claim | Notice region, explicit renew |
| One user has two tabs | Both show the user present until the last tab closes | Per-user connection count in the application |
| The application runs on several instances | Writes stay safe; presence needs shared storage | Database arbitration, a broker backend |

## Boundaries

| Tavern provides | Your application provides |
|---|---|
| Fan-out of invalidations and fragments | Identity, permissions and tenant checks on every request |
| Loss-aware subscriptions, replay and gap reporting | Canonical state, versions and conflict responses |
| Subscription status and delivery counters | Claims, leases, assignment and their expiry |
| A single-process presence tracker | Presence across instances and tabs |
| Snapshots and control events for recovery | Draft protection and reconciliation on the page |
