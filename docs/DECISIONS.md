# Decision Register

Every architectural decision, why it was made, and what it costs. Referenced by ID
throughout the codebase and the other documents. K1–K14 come from the original project
draft; K15–K28 were added after the multi-user / correctness review; K29–K57 resolve
the M1–M8 build.

A decision is only listed here if reversing it later would be expensive. Preferences
that can change freely do not belong in this file.

**Status legend:** `active` · `extended by Kn` · `superseded by Kn`

---

## Foundation

### K1 — The ledger is the only source of truth · `active`
`ledger_events` is append-only. Position, portfolio, PnL and risk are pure folds over
it. No engine keeps its own durable "truth"; what it keeps is a reproducible projection.

**Consequence:** the `positions` table is a cache. It must be droppable and rebuildable
from the ledger with an identical result, and there is a test enforcing that. This is
what makes historical reconstruction possible and what lets us recover from a
calculation bug instead of living with corrupted state.

### K2 — Bitemporal time · `active`
Every event carries two timestamps:
- `event_time` — what the exchange says happened, and the **only** time calculations use
- `ingested_at` — when we saw it, used for gap analysis, late-arrival detection, debugging

**Consequence:** "what was my portfolio at 2026-08-25T14:00Z" is a query over
`event_time <= T`, not a stored snapshot.

### K3 — Idempotent ingestion · `extended by K19`
REST backfill and the WebSocket stream deliver the same event. Inserting the same event
twice must produce the same ledger, and this is a tested invariant, not an intention.

**Original key was `UNIQUE (integration_id, source, external_id)` — that was wrong.**
See K19: including `source` in the key defeats the exact deduplication it was meant to
provide, because REST and WS report different `source` values for the same trade.

### K4 — Money representation · `active`
Postgres `NUMERIC(38, 18)`; Go `shopspring/decimal` mapped via sqlc `overrides`; JSON
**string**. `float64` never carries money, quantity, price, or fee — including during
serialization.

**Why 38/18:** 18 fractional digits covers wei-scale precision; 20 integer digits covers
any plausible notional. Binary floating point cannot represent `0.1` exactly, and in a
system whose entire claim is correctness, an off-by-a-satoshi total is a product failure.

### K5 — Exchange-style average cost · `active`
This is trading-position PnL, not tax accounting. Key: `(integration_id, instrument_id)`.
One-way mode only. A position can flip long → short in a single trade: realized PnL is
computed on the closed quantity, and the remainder opens in the new direction with a new
average entry.

**Constraint:** average cost may never be stored as the *only* truth. The ledger must
stay **lot-derivable** so a V2 tax-lot projection (FIFO/LIFO/HIFO, per-venue cost basis)
can be recomputed from events rather than bolted onto the schema. See K12 in
`COMPETITIVE-ANALYSIS.md` §5 (G12) for why this is a regulatory constraint, not a nicety.

### K6 — Liquidation price is read, not computed · `active`
We do not calculate it ourselves — it depends on margin-tier tables, cross vs isolated
margin, and wallet balance interactions that drift without notice. The exchange's
liquidation price is stored in the `position_risk` snapshot; **liquidation distance** is
what we compute, from the mark price.

**Trade-off:** we inherit the exchange's number, including its staleness. Acceptable,
because a confidently wrong liquidation price is far more dangerous than a slightly late
correct one.

### K7 — Price history is durable · `extended by K28`
Redis holds only the latest price. Historical valuation reads `price_ticks` in Postgres —
minute-resolution mark price is sufficient, not every tick. BRIN index on `ts`.
No TimescaleDB or ClickHouse in V1.

### K8 — Modular monolith, no broker · `active`
`cmd/api` + `cmd/worker`. Modules talk through Go interfaces, not the network. Kafka/NATS
are not added until a real need appears.

**Why:** the hard problems here are correctness problems, not throughput problems. A
message broker would add operational surface and distributed-ordering bugs to a system
whose value proposition is that its numbers are right.

### K9 — Credential security · `extended by K25`
API keys are **read-only**: withdrawal disabled, trading disabled. Permission is verified
when the connection is established, and an over-permissioned key is rejected rather than
accepted with a warning. Encrypted at rest with AES-256-GCM. Plaintext never reaches a
log.

### K10 — Asset is not Instrument · `extended by K22`
Most valuation errors are not price errors, they are **identity errors**: ticker
collisions, wrapped/bridged assets, the same symbol used in both spot and derivative
markets. `assets` (BTC, ETH, USDT — the canonical thing) and `instruments`
(BTC-USDT-PERP — the tradable thing) are separate tables. An exchange symbol is never a
key; it is always resolved through an alias table.

**Concrete case:** on Binance, spot `BTCUSDT` and perpetual `BTCUSDT` are the same
string and different instruments. Without this split the positions merge and every
downstream number is wrong.

### K11 — A single valuation policy · `extended by K17, K23`
Every portfolio and risk response is produced from one `valuation_run` carrying `as_of`,
`price_source`, and staleness. Producing two different totals from two different price
sources in one response is forbidden. Price-source priority is defined per instrument
(perp → exchange mark price, spot → exchange last/index) and the fallback chain is
recorded.

**Why:** "every screen shows a different total" is the failure that makes users believe
a product is lying to them. See `COMPETITIVE-ANALYSIS.md` §3.3.

### K12 — A transfer is not a sale · `extended by K49`
An asset withdrawn from one venue and deposited into another is **one transfer**.
Unmatched, the system reads it as a disposal plus an acquisition and PnL collapses.
`transfer_links` joins the two ledger events. Matching heuristic: same asset + amount
(within fee tolerance) + time window + txid when available. Unmatched pairs go to a
queue the user can resolve manually.

V1 covers intra-venue transfers (spot ↔ futures); M8 covers cross-venue. Verifying the venue
before building it moved most of this decision's machinery into M8 — see K49.

**Why it matters:** this is the entire industry's number-one support topic.

### K13 — Strategy is a first-class dimension · `active`
Positions and events carry an optional `strategy` tag. A delta-neutral basis trade
(spot long + perp short) that is not grouped under one strategy gets reported as
"2× leveraged, risky" when its net delta is ~0 — and the system then generates false
alerts forever. Exposure and leverage are computed at both portfolio and strategy level.

**V1 scope:** strategy is assigned at the **position** level — one strategy per position.
Splitting a single instrument's position across strategies is V2.

### K14 — Data quality is a visible feature · `active`
Every product shows numbers; our difference is being able to show *why* the number is
right. Continuously running checks, served by their own endpoint:

- **Negative balance** — the ledger implies selling more than was ever held ⇒ a missing event
- Sequence / WebSocket gap, missed keepalive
- Unknown asset or unresolvable symbol
- Price gap (the mark price stream stopped)
- Unmatched transfer
- Missing price for a fee asset at `event_time`
- Exchange clock skew beyond tolerance

---

## Multi-tenancy and access

### K15 — Multi-user from day one, with RLS as a backstop · `active`
The system serves multiple users in V1. Every tenant table carries `account_id`
**denormalized onto the row** — not reachable only through a join — because RLS policies
must evaluate against a column to stay fast and simple.

Two independent defences:
1. **Primary:** every query is scoped by `account_id` at the application layer.
2. **Backstop:** `ROW LEVEL SECURITY` enabled *and* `FORCE`d on all tenant tables; each
   transaction opens with `SET LOCAL app.account_id`.

Cross-tenant mixing is additionally impossible at the schema level via a composite
foreign key `(account_id, integration_id) → integrations(account_id, id)`.

**Two traps this decision must respect:**
- A table's **owner bypasses RLS**. Migrations run as the owner role; the application
  connects as a separate restricted role. `FORCE ROW LEVEL SECURITY` closes the rest.
- `SET LOCAL` only lives inside a transaction, so **every** tenant read runs in one.
  That cost is accepted deliberately.

**Cost:** all reads are transactional; a `tenancy` wrapper is mandatory boilerplate.
**Bought:** a forgotten `WHERE` returns an empty result instead of another user's
portfolio.

### K16 — Invite-based accounts, opaque revocable sessions · `active`
No open sign-up in V1. Accounts are provisioned by an administrator, which removes email
verification, password reset, bot protection and mail infrastructure from V1 entirely.

- Password hashing: **argon2id** (`x/crypto/argon2`)
- Session: 32 bytes from `crypto/rand`, stored in Postgres as a SHA-256 hash, delivered
  as an `HttpOnly; Secure; SameSite=Lax` cookie

**Explicitly not JWT.** A JWT cannot be revoked before it expires. "I deleted my API key
but the session lived another 15 minutes" is not acceptable in a system holding exchange
credentials.

---

## Correctness fixes found in review

These four correct defects in the original draft. Each would have required rebuilding
the ledger if discovered after implementation.

### K17 — USD numeraire, stablecoins are priced · `active`
All portfolio totals and equity are computed in **USD**. USDT and USDC are *not* assumed
to be worth 1.00 — they are priced like any other asset.

Because no direct USDT/USD pair exists, valuation walks a **price path**
(`asset → quote asset → … → USD`). The `valuation_run` records the path actually used
and sets `assumed_peg = true` whenever a leg fell back to a hard-coded 1.00.

**Why not USDT as the base:** it matches the exchange screen and simplifies
reconciliation, but if USDT depegs the system reports "everything is normal". A product
selling correctness cannot have a blind spot shaped exactly like its worst tail risk.

**Cost:** an extra price source and a pricing graph in `valuation`.

### K18 — A fee stays in its own asset · `active`
When commission is paid in BNB, the ledger keeps it as-is (`fee` + `fee_asset`).
`avg_entry_price` remains the clean execution price, so it matches what the exchange
displays and reconciliation stays free of unexplained deltas.

Fees are a separate realized-PnL component, converted to USD at the fee event's
`event_time`. If no price exists for the fee asset at that time, a data-quality finding
is raised (K14) rather than the fee being silently dropped.

**Also resolved:** the original draft defined a fee both as a column on
`ledger_events` and as a `FEE` event type — writing both double-counts it. The rule is
now: a fee belongs to the event that caused it; the standalone `FEE` type exists only
for fees with no parent event.

### K19 — Event identity is source-independent · `supersedes part of K3`
```
UNIQUE (integration_id, venue_event_id)     -- e.g. spot:trade:BTCUSDT:12345678
```
`source` (`binance_spot_rest`, `binance_usdm_ws`, …) is recorded as metadata — which
ingest path saw the event first — and is **never** part of identity.

**The defect this fixes:** K3's stated purpose was that REST and WS delivering the same
trade must deduplicate. But its key included `source`, and REST and WS carry *different*
source values, so the constraint never fired. Both rows would insert and every position
would be exactly double. Correct on paper, broken in the schema.

### K20 — One writer per integration; never project on global `seq` · `active`
`seq BIGSERIAL` cannot order commits. Sequence values are handed out *before* commit, so
a reader can see `seq=100` committed while `seq=99` is still uncommitted. A `last_seq`
projection cursor skips that event, and because the ledger is append-only (L2) it is
skipped forever.

**Resolution:**
- Projections advance **per `integration_id`**, never over the global sequence.
- Exactly one worker owns an integration at a time, held by a Postgres lease row
  (`owner_id`, `heartbeat_at`, `expires_at`), which makes the per-integration stream
  single-writer.
- `seq` is retained for debugging and lineage display only.

### K21 — Canonical event ordering · `active`
```
ORDER BY event_time, venue_sequence, venue_event_id
```
Ordering by `event_time` alone is undefined: the fills of a single order share one
millisecond timestamp on Binance. Under an undefined order, `avg_entry_price` depends on
ingest order and the order-independence test (`docs/PROJECT.md` §10) becomes flaky in a
way that looks like a heisenbug rather than a schema gap.

`venue_sequence` stores the exchange's own trade/event id as a sortable value and is a
required column, not an optional one.

### K22 — Aliases are time-scoped · `extends K10`
`asset_aliases` and `instrument_aliases` carry `valid_from` / `valid_to`, with an
exclusion constraint preventing overlapping windows for the same external symbol.
Resolution always happens **at the event's `event_time`**, never with today's mapping.

**Why:** exchanges recycle symbols after a delisting. With an untimed alias table, a 2024
trade resolves to the 2026 instrument — a correct quantity attached to the wrong
identity. This is precisely the failure mode described in `COMPETITIVE-ANALYSIS.md`
§3.1, where an identity error presents itself as a price error and is debugged in the
wrong place for weeks.

### K23 — Structured freshness replaces the `stale` boolean · `extends K11`
A single `stale: true` flattens unrelated conditions — WS gap, stale price, incomplete
backfill, assumed peg, open reconciliation finding — into one bit that tells the user
nothing actionable.

Every response instead carries:
```
freshness: {
  status:  ok | degraded | unreliable,
  reasons: [ { code, severity, detail, since } ]
}
```
For a product whose pitch is "I can show you why my numbers are correct", this field is
the pitch made concrete. It is API surface, not diagnostics.

---

## Operational

### K24 — Two-tier rate limiting · `active`
Binance enforces weight budgets **per API key** *and* separate limits **per IP**, and
caps WebSocket connections per IP. On a single VPS every user shares one IP, so
per-key accounting alone leaves the key budgets healthy while the IP gets banned.

- **Tier 1:** per-integration weight budget
- **Tier 2:** a process-wide shared per-IP budget every outbound call passes through
- WebSocket connections are handed out by a quota-aware connection manager

Implemented with `x/time/rate`; `WaitN(ctx, weight)` models variable endpoint cost
directly.

**Work priority** is fixed and not negotiable at runtime:
`realtime > reconciliation > backfill`. Backfill is chunked and interleaved so one
user's full-history import cannot starve another user's live stream.

### K25 — Envelope encryption for credentials · `extends K9`
Each account has its own DEK; the DEK is wrapped by a master KEK. Rows carry
`key_version`.

**Bought:** a single account's key can be rotated without touching everyone else's, and
a leak has a bounded blast radius. Encrypting everything directly with one master key
has neither property.

The master key comes from a `KeyProvider` interface. V1 implements it from an env file
(`0600`, outside the repo); moving to KMS is one new implementation, not a migration.

### K26 — Full-history staged backfill · `active`
Backfill goes back to the account's first trade, not a fixed window.

**Why not one year:** an asset acquired outside the window and sold inside it makes the
ledger produce a negative balance, which trips the K14 check permanently. Working around
that requires an opening-balance concept — an entire synthetic-event subsystem invented
purely to paper over missing history, and one that permanently poisons cost basis.

Backfill is chunked with persisted progress so it resumes where it stopped. Until it
completes, the portfolio is served with `backfill_incomplete` in `freshness` (K23) —
visible, never silent.

### K27 — Single VPS, same-origin, Caddy · `active`
Docker Compose on one server; the same compose file locally and in production, differing
only by override. Caddy terminates TLS and serves the frontend and API **under one
origin**.

**Consequence, and the reason:** no CORS configuration exists, and the session token is
never reachable from JavaScript. A cross-origin setup would require CORS rules plus a
token storage strategy — two more chances to leak credentials, bought for nothing.

### K28 — Redis holds only rebuildable state · `extends K7`
Redis has exactly two jobs: the latest-price hash and SSE fan-out pub/sub. Losing Redis
degrades latency; it never loses data. Anything durable is in Postgres.

Reviewed for removal — Postgres `LISTEN/NOTIFY` could cover the fan-out — and kept,
because two genuine jobs justify the component. If it ever drops to one, remove it.

### K29 — `integrations` lands with the ledger, not with credentials · `active`
`ledger_events` carries the composite foreign key
`(account_id, integration_id) REFERENCES integrations (account_id, id)`, which is what
makes attaching an event to another account's integration impossible at the storage layer
rather than merely unlikely. That FK cannot be added to a populated ledger without
rebuilding it — the exact rebuild M0 existed to make unnecessary — so the table ships in
M1 with identity and tenancy only. The credential columns arrive in M2 with envelope
encryption (K25).

**Cost:** a table that holds nothing useful for one milestone. Cheaper than the
alternative by a wide margin.

### K30 — The strategy tag does not live on `positions` · `supersedes a line in PROJECT.md §2`
`positions` is a projection: droppable, and rebuilt from events (L3). The strategy
assignment is **user input**. A tag stored on the projection is erased by every rebuild —
and the rebuild-equality test would still pass, because both sides would be equally empty.
It lives in `position_strategies`, keyed the same way and never touched by `Rebuild`.

**The general rule:** anything a human typed cannot be stored on a table that is derived.
This applies again to manual transfer confirmations (K12) and to acknowledged
reconciliation findings.

### K31 — M1's fixtures are canonical events, not exchange payloads · `active`
The golden files in `testdata/golden/` hold normalized events plus the arithmetic worked
out by hand. Normalization is the exchange module's job in M2, and recorded Binance
payloads live in `testdata/fixtures/binance/`. Mixing them would make a golden failure
ambiguous about which layer broke.

**Why the arithmetic is written in the file:** a golden file that records only what the
code produced proves the code is deterministic and nothing about whether it is right —
which matters here because the same author writes the engine and its tests.

### K32 — The ledger writes and the fold live outside the pure engines · `qualifies L4`
L4 names `ledger` and `position` as pure functions with no database handle, while the
repository layout gives `ledger` the append-only writes. Both cannot be true. The
resolution:

- `ledger` holds a **transaction-scoped** `*store.Queries` handed to it by `tenancy.InTx`
  — never a pool, a driver, a clock or a logger. A depguard rule of its own states this.
- `position` stays literally pure. The projector that folds the ledger into it lives in
  `internal/projection`, which may reach Postgres only through `tenancy.InTx`.

**Why not simply relax L4:** the law's value is that it is checkable. Written down as
"engines are mostly pure", it stops catching anything.

---

### K33 — F5 is checked at runtime, not assumed · `active`
The documentation states that `myTrades?fromId=N` returns trades with id ≥ N, and that the
24-hour limit binds `startTime`/`endTime`. It does not state what `fromId=0` returns with
no time range. The plan inferred "the oldest trades"; the inference is not verified and no
key exists to settle it.

Abandoning `fromId` is not the safe alternative: a pure 24-hour walk from spot's 2017
launch is roughly 3,300 windows per symbol at weight 20, which is not a backfill anyone
runs. So the walk keeps `fromId` and **checks the inference before its first page** — two
probes, and a per-page contiguity check after — stopping with `backfill_incomplete` if
`fromId=0` turns out to anchor at the newest trade (L11).

**Why this shape:** the failure it guards against is silent. A walk that assumed wrongly
would read one page, find nothing after it, and record a complete history missing
everything before — with plausible numbers and nothing to say so.

---

### K34 — Withdrawals are not normalized until their enum is published · `active`
`NormalizeWithdrawal` does not exist. Two facts it needs are undocumented, both checked
twice on 2026-09-04: the withdraw **status enum** appears only as the garbled fragment
`0(0 Sent, 2 Approval 3 4 6)`, and the **timezone** of `applyTime`/`completeTime` — which
arrive as `"2019-10-12 11:12:02"` rather than epoch milliseconds like every other endpoint
— is stated nowhere.

Which status means "completed" decides whether coins are recorded as having left the
account, and an eight-hour timezone error corrupts the canonical order (L7) and every
time-windowed reconciliation. The `withdrawals` scope name is reserved in migration 00013
so that adding the walk later is code rather than a migration.

**Why not guess:** encoding a remembered enum into append-only financial rows is precisely
what `CLAUDE.md` §2 forbids, and the ledger cannot be corrected by an UPDATE (L2). Because
`raw` is stored verbatim (L15), a later fix is a replay rather than a migration.

---

### K35 — The gap replay reads by time; the walk reads by id · `active`
They are two strategies rather than one with an optional parameter, and that is forced.
`rest-api.md` enumerates the parameter combinations `myTrades` accepts, and
`symbol + fromId + startTime + endTime` is not among them. A windowed page that comes back
full is therefore **halved** rather than paged, because there is no supported way to ask
for the rest of that window.

The replay also moves no cursor. The walk owns "how far back have we read"; a replay of ten
minutes that touched that cursor would declare a symbol's whole history complete. Overlap
is free instead: dedup on venue identity (L5) means a generous window costs requests and
stores nothing twice.

---

### K36 — The worker's integration list is protected by privilege, not by RLS · `extends K15`
A worker has to know which integrations to run before it knows whose they are, and RLS
answers every cross-account question with an empty result. A `SECURITY DEFINER` function
over `integrations` does not help either: `FORCE ROW LEVEL SECURITY` binds the owner too,
so the function would run as the owner and still see nothing.

So `integrations` keeps `FORCE ROW LEVEL SECURITY` and L12 needs no exception. Migration
00015 adds `worker_integrations`, a lookup index of `(account_id, integration_id,
runnable)` maintained by trigger, carrying no policy and no grant to `plimsoll_app` — the
same shape `account_credentials` takes for login (00003), and the same rule that migration
states: **anything reachable before authentication is protected by privilege, anything
reachable after it is protected by RLS.**

**Why a trigger:** an index a future writer can forget to update is an index that will be
wrong, and being wrong here means an account whose trades are never ingested and nothing
that says so.

---

### K37 — Losing the lease is enforced inside the write, not around it · `extends L6`
"A worker that loses its lease writes nothing further" is a promise a flag cannot keep:
between checking the flag and committing the write there is a window, and a lease exists
precisely to say there is no such window.

`worker.GuardLease` runs inside the same transaction as the write it protects, so a stale
worker's events roll back with the guard that refused them. The property belongs to the
database rather than to a code path.

**How this was found:** the mutation that removed the guard survived, because the test was
letting the guard and the heartbeat cover for each other. Neither was actually proven. It
is now two tests — one with the watchdog disabled so only the guard can act, one with no
event to refuse so only the heartbeat can.

### K38 — The fold runs in the worker, on a ticker, under the same lease · `extends K20`

M2 shipped a projector nothing called. `projection.Project` was written, tested and
rebuild-equal; no process ran it. On a live system the ledger filled and `positions`
stayed empty, and the first endpoint to read it would have answered "you hold nothing"
for an account with a full history. Found while planning M3, not by a test — which is
itself worth recording: every test in the package called `Project` directly, so none of
them could notice that production never did.

**Where it runs: the worker.** The fold is a write, and the single-writer lease exists so
that exactly one process writes for an integration (K20, L6). Folding on read would put
every API replica in that role at once, each advancing the same per-integration cursor.

**When it runs: a ticker, not per event.** One fold is a transaction over every touched
instrument. Running it per fill during a busy minute buys nothing — nobody read the
number in between. Two seconds is chosen from the reader's side: the longest a portfolio
may silently lag a fill before the lag costs more than the transactions saved.

**What the interval costs, and who is told.** A portfolio read can be up to one tick
behind the ledger. That is reported, not hidden: the API compares the projection cursor to
the ledger and raises `projection_lagging` (L11).

**A failed fold does not stop the ingestion.** The asymmetry is deliberate. Live events are
the one thing that cannot be recovered — a stream nobody is reading is data gone. A
projection is by definition rebuildable (L3). Killing ingestion because a projection failed
trades a permanent loss for a temporary one. The reader still learns of it, because a
projector that is failing and a projector that is behind look identical from the outside,
and both raise the same reason.

---

### K39 — The worker publishes its state; the API does not ask for it · `extends K23`

The supervisor already knows whether it is live, degraded, resyncing or backfilling, and
already turns that into a freshness reason. It knows it *in the worker process's memory*.
The API is a different process, and on a fleet a different machine.

Without somewhere to put it, a portfolio response has no way to say "the live feed for
this integration is down" — and would answer with numbers that look current because
nothing contradicted them. That is the exact failure L11 exists to reject, so the state is
published to `integration_status` rather than inferred.

Three properties make the table honest rather than decorative:

- **`since` moves only when the state changes.** A heartbeat republishing "degraded" every
  forty seconds must not keep resetting how long it has been degraded, or an hour-long
  outage reads as a fresh blip every time anyone looks. Enforced in the upsert, not in Go.
- **`updated_at` moves on every heartbeat.** It is how a reader tells a worker that is live
  now from one that was live when it died. A report older than one lease TTL is stale, and
  a stale report is worse than any state it names.
- **The publish is lease-guarded, inside its own transaction** (K37). A worker that lost
  its lease stops describing an integration it no longer writes. Otherwise a dead worker's
  "live" outlives it in the row every portfolio response is built from.

An integration with **no** row is not "connecting" — it is "nobody is ingesting this",
which is why the reader's query is a LEFT JOIN from `integrations` rather than a select
over the status table. An inner join would hide precisely the case that matters most.

This table is not a projection and not a source of truth: it is one writer's report about
itself. Losing every row costs the reader its freshness detail and nothing else.

---

### K40 — A portfolio with no price source has subtotals, not a total · `extends K11`

M4 owns prices. M3 has none, so `GET /portfolio` reports what the fold produces without
one: quantity, average entry, cost basis, realized PnL, fees.

**No total field exists**, not even a null one. Realized PnL on BTC-USDT is denominated in
USDT and on ETH-BTC in BTC; a field adding them holds a number with no unit, which is the
"every screen shows a different total" failure in miniature. What is reported instead is a
subtotal per quote asset, and the type is named `QuoteTotal` so no later caller mistakes it
for the other thing.

A missing field alone would be silence, and a client finding no total would reasonably
guess the account is empty. So `valuation_unavailable` says why (L11). It is a **warning**,
not an error: every number present is exact. Marking an exact response `unreliable` erodes
what `status` means as surely as failing to mark a wrong one — and `status` is only worth
reading if it has stayed honest in both directions.

---

### K41 — The ledger is paginated in canonical order, and the page is not final while a backfill runs

ARCHITECTURE.md §10 originally said cursor pagination on `seq`, "stable because the ledger
is append-only". Append-only is not enough. `seq` is assigned before commit, so a reader
can pass a value still in flight and skip it permanently — the same hazard L6 forbids for
projections, and it does not become safe because the reader is a person.

So `GET /transactions` pages on `(event_time, venue_sequence, venue_event_id)`, the same
keyset the fold reads in (L7). That fixes the in-flight hazard and does **not** fix the
other one: a backfill inserts *behind* a cursor a reader has already passed. No ordering
avoids that while history is still loading, because the rows genuinely arrive late.

The honest answer is not a better cursor, it is saying so: a listing carries the same
`backfill_incomplete` reason the portfolio does, and a client is told its page is a view of
an incomplete set rather than a final one. `seq` stays in the response as lineage — it is
what a support conversation quotes — and never as a cursor.

---

### K42 — A position's API id is its natural key · `extends L3`

`positions` is a projection: it can be dropped and folded again to the same rows, and the
rebuild-equality test requires exactly that. A `BIGSERIAL` id would come back different from
every rebuild — breaking every link a user saved, every alert that named a position, and
every id a support conversation quoted.

The id is therefore `<integration_id>.<instrument_id>`: the key the projection is already
stored under, which is identical before and after a rebuild by construction. The separator
is a dot because neither half can contain one — a UUID is hex and hyphens, an instrument id
is digits — so parsing is unambiguous without escaping.

The cost is a longer, uglier id in a URL. That is the whole cost, and it buys an identifier
that cannot silently start pointing at a different position.

---

### K43 — Lineage checks itself, and reports when it disagrees · `extends L3`

`GET /positions/{id}/lineage` replays every event that folded into a position, through the
same `position.Apply` the projector runs, and shows the state each one produced. It then
compares the end of that replay against the stored projection row.

If they differ, the response carries `lineage_mismatch` at severity `error`. It is the one
reason code that accuses the system itself rather than the venue or the network, and it is
unqualified: the product claim is that the numbers are right and we can prove it, so a
proof that comes out different is the most serious thing this API can report. Serving the
number silently, having just disproved it, is the failure L11 exists to name.

**It only fires when both sides end on the same event.** A projection that is behind is
normal — the fold runs on a ticker (K38) — and is already reported as `projection_lagging`.
Calling that a disagreement would make the serious signal fire constantly and stop meaning
anything, which is how a warning becomes wallpaper.

**The cost:** a replay is the position's whole history, per call. That is honest for M3 and
is what `position_snapshots` exists to fix (ARCHITECTURE.md §3). The invariant there —
`snapshot(T) + events(T, T'] == full_fold(T')` — is the same equality this endpoint checks,
which is not a coincidence: a snapshot is a cache, so adding one must change the timing of
this check and never its answer.

---

### K44 — Balances are the other fold, and negative is a finding rather than a bug · `extends L3`

M3 shipped a portfolio made of positions. That is the right answer for a derivatives
account and half of one for a spot account: "long 0.5 BTC at 60000" does not say how much
USDT is left. `asset_balances` is the other fold over the same events — a buy acquires the
base and *spends the quote*, and a fold that only credited the base would show free money.

It folds **beside** the positions, in the same transaction, on the same cursor. Two
cursors over one event stream are two chances to disagree about what has been folded, and
the disagreement would be silent (L6). It gets its own rebuild-equality test for the same
reason positions have one: a balance that survived a rebuild only because nothing checked
it is exactly the second source of truth L3 forbids.

**The quantity column has no non-negative CHECK, on purpose.** A negative balance is not a
storage error — it is K14's strongest data-quality signal: the ledger implies selling more
than was ever held, so an event is missing, and the check finds that without knowing what
the missing event was. A constraint forbidding it would turn the finding into a crash and
lose the evidence. It surfaces as a field on the row *and* as `negative_balance` at
severity `error`, so a client that reads nothing but `status` is still warned.

The half that makes the check worth having is that it goes quiet: an account whose
deposits pay for its fills raises nothing. A check that fires on every account is not a
check.

---

### K45 — The fee's asset is resolved at ingest, and an unknown ticker costs the fee, not the fill

`fee_asset` holds the exchange's ticker, and a ticker is not a key (K10, K22). To move a
balance we need the asset, and resolving it at **fold** time would resolve it with whatever
mapping is current then — the industry's number-one silent corruption. `fee_asset_id` is
therefore written at ingest, which is the only moment the event's own `event_time` is
unambiguously in hand (L8).

The column is nullable and must stay so. A ticker the registry does not cover is a real
possibility, and rejecting the trade over it would **lose a fill to keep a fee** — trading
the irreplaceable half for the replaceable one. So: the trade stores, `fee_asset_id` stays
NULL, the balance fold refuses to guess, the projector drops that one fee's balance effect,
and the reader is told through `unknown_symbol`. The shortfall is exact, bounded by the fee
itself, and visible.

**Only `asset.ErrUnknownSymbol` is swallowed.** A resolver failing for any other reason —
the database unreachable, say — still fails the normalization. Silently dropping every fee
attribution for the duration of an outage is not the same thing at all, and it would leave
nothing to find afterwards. The mutation that widened the swallow to every error is killed
by a named test.

---

### K46 — Migrations are a step in the topology, not a thing an operator remembers · `extends K15`

A clean machine could not bring the stack up. `make up` started the worker against an
empty database, and it exited on `function worker_active_integrations() does not exist`.

The interesting part is not the fix, it is why it survived so long. Every run until then
had a database somebody had already migrated by hand, so the missing step was invisible on
exactly the machines that were doing the testing and fatal on every machine that was not.
That is the shape of a deployment bug: it cannot be found by the people who already have
the state, and the failure it produces names a missing *function* rather than a missing
*step*.

**migrate is a one-shot service the others wait on.** The api and the worker depend on it
with `service_completed_successfully`, so the schema is current before anything reads it.
An ordering that is a property of the topology cannot be forgotten; a line in a runbook
can.

**It runs `plimsollctl migrate`, with the migrations embedded in the binary.** Two things
follow, and both are the point:

- The image and the schema it applies cannot be different revisions of each other. A
  container migrating from a mounted directory can be pointed at the wrong revision, and
  that failure surfaces later as a missing column rather than as the deployment mistake it
  was.
- It runs as `plimsoll_owner`, never the app role, for the same reason goose does:
  migrations are DDL, and the role that serves requests must not hold DDL rights (K15,
  L12). The one-shot is the only container in the topology that connects as the owner.

The embed is guarded by a test that compares the embedded set against the directory by
name **and by contents** — two files can share a name and differ in every byte, which is
exactly what a stale embed is.

### K47 - The total is the sum of the balances, and it names what it leaves out · `extends K11`

M4 gave the portfolio a total, and the question that took the longest to answer was not how
to compute it but *what to add up*. Two candidates were on the table and only one of them is
a portfolio.

**The total is the valued balances, never the positions' market values.** A position and the
balance its fills moved are two views of one trade: buying 0.5 BTC creates a BTC position
and a BTC balance out of the same event. Summing both counts the account's money twice, and
the resulting number is wrong in a way that looks plausible on a screen -- roughly double,
which reads as "leverage" to anyone who does not already know the bug. Positions still get
`market_value` and `unrealized_pnl` individually, because "what is this exposure worth" is a
real question; they are just not what the account is worth.

**A total that excluded something says so.** An asset with no route to USD is named in
`unpriced_assets` and raised as `unknown_symbol` at error severity. "Your portfolio is worth
X" and "worth X, minus the part we could not price" are different sentences and only one of
them is true.

**A run that priced *nothing* the account holds produces no total at all.** The empty sum is
arithmetically defensible and reads on screen as an empty account. That is the same failure
`valuation_unavailable` was written to prevent, arriving by a different route, so the field
is absent rather than zero -- and absent is an empty string, never `0`, for the reason L1's
`nullText` exists.

**`valuation_unavailable` was narrowed rather than retired.** `ARCHITECTURE.md` said "M3; M4
removes it", and that was half right. The reason keeps exactly one job: no run has completed.
On a fresh install whose feed has never connected there genuinely is no valuation, and a
release that deleted the code path in the name of cleanup would have shipped a confident zero
to the one user least able to tell it was wrong.

**Price staleness is measured against the read time, from the run's worst leg.** The worst
and not the average, because a total is only as current as the oldest price inside it and an
average hides one forgotten instrument behind a hundred fresh ones. Against the read time and
not the run's own `as_of`, because a run produced an hour ago from prices that were fresh
when it ran is stale *now*, and now is when the reader is asking.

The tolerance is sized by how prices are produced rather than by how fast a market moves: the
stream reports only tickers that changed (F7), so a quiet instrument's age is bounded by the
worker's REST re-snapshot. Twenty minutes therefore means "the snapshot loop has stopped",
which is worth a warning; a tighter value would only report that some listed pair is quiet,
which is not news, and a warning a reader learns to ignore is worse than no warning at all.

---

### K48 - A past answer is rebuilt, not stored, and is a pure function of its instant · `extends K11, K47`

`GET /portfolio?at=T` folds the ledger to T and prices it from a run reconstructed out of
`price_ticks`. The run is **not written**, and it does not need to be: the ledger is durable,
the fold is pure and the price walk is pure, so the same T yields the same answer forever.
Persisting one would grow a table by a row per curious click and add nothing an audit could
not already reproduce.

**Never reaching forward.** The prices come from the newest tick at or *before* T. A gap in
the ticks around T therefore surfaces as an old `observed_at` -- reported as `price_stale` --
and never as the next price after the gap. Reaching across a gap values the past with
information nobody had at the time, and in a risk product that is the bug that makes a
backtest look brilliant and a liquidation arrive unannounced. A future `at` is refused for
the same reason wearing a friendlier costume.

**The response carries no reason that is a fact about now.** `ingest_stalled` says no worker
is reading an integration *at this moment*; a response about last Tuesday that carried it
would be answering a question nobody asked, with a `since` that moves every time the same
instant is requested. That is the one thing a reproducible answer cannot do, so the
historical read carries only conditions that are properties of T -- and a client that wants
the state of ingestion asks for the live portfolio, which is one request away.

`projection_lagging` is absent for a different reason: this endpoint folds the ledger itself,
so nothing can be behind it. The reason would be a lie rather than an irrelevance.

**Two runs in one PnL response is what L10 protects, not a breach of it.** `GET /pnl?from&to`
carries a run at each end. The law forbids one *number* built from two price sources; here
realized comes from the ledger and needs no price at all, and every marked figure names the
end it came from. An interval is two questions, and answering both from one instant's prices
would be the actual error.

**One thing a rebuilt run cannot promise, and says so.** Its peg set is today's configuration
(K17). The response marks it `rebuilt: true` and keeps `assumed_peg`, so a reader comparing a
reconstruction against a recorded run can tell which is which: one is what we said at the
time, the other is what the ticks say now.

**`GET /portfolio/history` is deliberately not shipped yet.** Its cost is N boundaries times a
full fold of the account's history, and the plan made it conditional on a measurement rather
than a guess. There is no real-sized ledger to measure against -- M2 is still waiting on a
read-only key -- and timing a four-event fixture would be dressing an assumption up as
evidence. It lands when there is a history worth folding, which is also when
`position_snapshots` stops being anticipated and starts being necessary.

---

---

---

### K49 - An intra-venue transfer names its endpoints, and folds to nothing · `extends K12`

K12 describes a matching problem: two ledger events, joined by `transfer_links`, with a
heuristic on asset, amount, time window and txid, and a manual queue for what the heuristic
cannot resolve. Verifying the endpoint before building it (F10) showed the intra-venue case is
not that problem at all.

`GET /sapi/v1/asset/transfer` returns **one row per transfer**, and its direction lives in the
required `type` parameter: `MAIN_UMFUTURE` and `UMFUTURE_MAIN` are two separate queries. The
row *is* the movement and it names both wallets. There is nothing to match, so `transfer_links`,
the unmatched queue and the manual-resolution endpoint are M8's, for the venues that genuinely
report two halves. Building a resolution UI for a problem this endpoint does not have would
have been the expensive kind of thoroughness.

**The endpoints go on the event; the sign comes off it.** `ledger_events` gained
`transfer_from` / `transfer_to` over a closed wallet vocabulary — `spot · usdm · coinm ·
margin · funding · external` — and the balance engine's signed-quantity convention is retired.
With the direction on the endpoints, a sign is a second statement of the same fact, free to
disagree with the first, and the disagreement would be silent: a withdrawal of -500 would read
as money arriving. A negative quantity is now refused by the schema, the normalizer and the
fold.

**A transfer between two wallets of one integration produces no deltas.** `asset_balances` is
keyed per integration, not per wallet, so moving USDT from spot to futures leaves the account
holding exactly what it held. The event stays in the ledger because it is history and lineage;
it is simply not arithmetic. Read as a disposal, it invents a realized loss — and then a
phantom re-purchase when the money comes back.

**`external` is in the vocabulary from the first day, and that is what makes the rest
testable.** Nothing writes one until M8. But a rule that only ever ran on the internal case
would have quietly become "a transfer moves nothing" — and, worse, without an external
transfer reaching the database a projector that skipped `TRANSFER` events entirely would
produce numbers *identical* to one that folds them correctly. No test could have told them
apart. The branch is not speculative generality; it is the only available observer of the case
it sits beside.

**The visible half.** Because a transfer changes no total, the transactions listing is the
only place a reader meets it. `GET /transactions` therefore carries both endpoints, and the
milestone's exit test asserts four things at once: the position is unchanged, the realized PnL
is unchanged, the balance is unchanged, and the transfer is *there*. A milestone whose success
is mostly the absence of change needs that fourth assertion, or a system that dropped the event
on the floor would pass.

### K50 - An unknown margin buffer is not a margin buffer of zero

`GET /risk` reports per integration and never sums across them, and an integration whose
capture is missing is **omitted from the response** while raising `collateral_unavailable` at
error severity.

The alternative -- one account-level total, with a missing integration contributing zero --
fails in the worst available direction. A margin buffer of zero says liquidation is imminent;
an unknown one says nothing at all. They are opposite claims, and a screen that renders them
identically is wrong at exactly the moment it is being read hardest. Summed into a headline
number the failure is worse still, because the omission is invisible: the total simply looks
smaller, and nothing on the page says an integration is missing from it.

**A stale capture is served, not withheld.** `collateral_stale` is a warning and the numbers
come with it. An hour-old liquidation distance is still the best answer anyone has, and
withholding it in favour of nothing would trade a qualified answer for a blank -- which is the
same mistake with better manners. The `since` on the reason is the capture's own instant,
which is knowable exactly: it is when the numbers stopped being current, not when we noticed.

**`as_of` is the OLDEST capture in the response, not the newest.** A screen is only as current
as its stalest number. Dating the whole of it by its luckiest one is how a response comes to
claim a currency it does not have -- the same rule that makes a snapshot's own `as_of` the
earlier of its two halves (F14).

**The capture loop and the endpoint that reads it shipped in one change.** Task 5 wrote the
capture and deliberately left it unwired; wiring it a task later, beside `GET /risk`, is what
made the test possible that names neither -- run the supervisor, read the endpoint, see the
buffer. M2 shipped a projector nothing called (K38) and the symptom was an endpoint answering
"you hold nothing" for a full account. A capture loop with no reader is the same defect facing
the other way, and it is invisible for exactly as long.

### K51 - SSE fans out over LISTEN/NOTIFY, and Redis waits for a second replica · `qualifies K28`

K28 gives Redis exactly two jobs: the latest-price hash and SSE fan-out. M4 put prices in
Postgres and never needed the first, so adding the container now would be a service run for
one job.

`LISTEN/NOTIFY` does that job on a single-VPS deployment with one `api` process: the worker
notifies on its own transaction, the API holds one dedicated connection per account with a
subscriber, and nothing durable is involved -- which is the whole of what K28 requires of the
transport. The day a second `api` replica exists this becomes wrong in a specific and visible
way (each replica only hears what its own connection is listening for), and that is the day
Redis earns its container.

**The payload is a hint, never the numbers.** A subscriber is told "portfolio changed" and
re-reads through the authenticated endpoint. Pushing the numbers down the channel would put a
second, unversioned copy of the API contract on the wire -- one that no `freshness` envelope
travels with, and a second place for a tenancy mistake to live.

### K52 - The dashboard parses no number · `extends L1`

Every amount the API sends is a string, and the dashboard keeps it one: grouping, trimming and
percentages are string edits, and there is no `parseFloat`, `Number()` or `toFixed` anywhere in
the tree. A script in `make frontend-check` fails the build if one appears.

The reason is that JavaScript has no other number. `NUMERIC(38,18)` holds more than a float64
can, so `12345678901234567890.123456789012345678` comes back as `1.2345678901234568e+19` --
right in the database, wrong on the screen, and wrong in the direction nobody checks. The whole
backend is built to keep those digits; losing them in the last thirty pixels would undo it.

**Absent and zero render differently.** The API sends `""` for a number it could not compute --
an unpriced position, a margin buffer with no capture behind it -- and the dashboard renders a
dash. Rendering `0` there would make the opposite claim to the one the response is making, which
is K50's rule arriving at the last layer that can break it.

**The freshness banner is the first component, not the last.** Every page renders it above the
numbers, structurally: the page component takes the endpoint and renders the envelope before it
renders anything the envelope qualifies. Bolting it on afterwards is how a dashboard ends up
with totals on three screens and qualification on two.

**There is no authentication code.** The session cookie is HttpOnly and same-origin (K16, K27),
so the browser attaches it and a script cannot read it. A 401 is the only thing this side can
observe about a session, and the answer to one is the login form.

---

### K53 - A finding has a lifetime, not a timestamp · `active`
Reconciliation runs every five minutes. A balance that is wrong all day is **one** problem.
If each run inserted a row, the register would grow by 288 rows a day per subject, and the
one question the user actually asks -- *is it still wrong?* -- would become a query over
history.

So a finding has identity `(account_id, integration_id, kind, subject)` and a lifetime: a
run **opens** what it newly sees, **touches** what it still sees, and **closes** what it no
longer sees. A partial unique index enforces the identity only `WHERE closed_at IS NULL`, so
a problem that returns after closing is a new finding rather than a resurrection -- "wrong
for an hour, right for a day, wrong again" is two incidents, and reporting it as one loses
the recovery in between.

This is deliberately the same shape as K49's alert hysteresis, because it is the same
failure: a repeating condition must not become a repeating record.

The register is UPDATEd, and that is allowed: it is a projection, not the ledger (L2, L3).
Drop it and the next run rebuilds it. Nothing may DELETE from it -- a finding closes, it does
not vanish, because the history of a finding is the evidence for the classification.

---

### K54 - Tolerance is per metric, and the classifier uses evidence rather than sign · `active`
One epsilon cannot serve both a quantity in BTC and a value in USD: pick a number small
enough for USD and every dust balance is a finding; pick one large enough for BTC dust and a
$400 discrepancy is invisible. Tolerance is therefore per metric -- quantity against a
per-asset dust threshold, value against a basis-point band.

The classifier is the harder half. The obvious mapping -- *they have more than us, so we are
missing an event; we have more than them, so we counted one twice* -- **is wrong**, because
"we have more than them" is equally explained by a withdrawal we never ingested (and
`NormalizeWithdrawal` deliberately does not exist, F5). Sign alone decides nothing.

Three classes are decided from evidence, and the fourth is the honest residual:

| Class | Decided by |
|---|---|
| `rounding` | the delta is smaller than one step of the subject's own precision |
| `unsupported` | our ledger holds a record for this subject that we deliberately do not normalize, large enough to explain the delta |
| `duplicate` | two events with the same venue identity under different integrations -- the one way L5's dedup key can still let a trade in twice |
| `missing_event` | **residual**: outside tolerance, and nothing above explains it |

`missing_event` as the residual is correct rather than lazy. It is the class that means *we
cannot account for this*, which is precisely what the user needs to be told.

---

### K55 - Resync re-runs the walk; it never writes a correction · `active`
V1 policy is detect and report, never auto-correct. "A resync action" must not be read as
automatic repair. Resync re-opens a backfill scope's cursor so the history walk runs again;
anything genuinely missing is appended by the ordinary ingest path under the ordinary dedup
key, and anything already present is deduplicated away (L5, K26).

It writes no correction event and touches no `ledger_events` row (L2). Auto-correcting a
**misclassified** finding writes a wrong correction into an append-only ledger, and that
cannot be undone. Automatic correction waits for V2, and only after K54's classification has
been validated against a real account -- which is one more thing riding on the key M2 is
waiting for.

---

### K56 - A shock names its asset, and a missing requirement is not a small one · `active`
A scenario is a statement about specific assets. "BTC minus twenty percent" must not move ETH:
inventing a correlation the user did not ask for produces a number that looks like analysis and
is a guess.

The corollary matters more than the rule. A shock hits the spot holding and the perpetual
**together**, because they are the same asset. A user long spot BTC and short BTC perp is
hedged, and a scenario that moved only one leg reports a loss they do not have -- the same false
alarm K13 exists to prevent, arriving through a different door.

The second half of this decision is what happens when something is missing. An unpriced holding
and an uncaptured bracket table are both absences, and they are handled differently on purpose,
because they fail in opposite directions:

| Missing | Effect | Treatment |
|---|---|---|
| a price for a holding | excluding it **understates** equity, overstating the danger | excluded, and named |
| a bracket table for a position | summing only the requirements we know **understates** the requirement, overstating the buffer | the buffer is unavailable entirely |

Partial answers are allowed where they are conservative and forbidden where they flatter. A
projection that reported an account as safer than it is, at the moment it is being asked whether
it is safe, is worse than no projection.

Maintenance is recomputed from the venue's tier table at the shocked notional, never scaled from
today's figure (F15). A shock large enough to be worth modelling is usually large enough to
cross a tier, and scaling assumes a constant rate -- wrong, again, in the direction that
understates the danger.

---

### K57 - A cross-venue transfer is matched, and an unmatchable half is a finding · `extends K12`
K49 found that the intra-venue case has nothing to match: Binance reports one row that names
both wallets. The cross-venue case is the problem K12 actually described -- two ledger events
under two integrations, joined by a heuristic.

The heuristic is asset, amount within a fee tolerance, and a time window, with `txid` deciding
outright when both halves carry one. The order matters: **a txid match is proof and is taken
first**, because two chains agreeing on a transaction hash is not a coincidence, while an
amount-and-window match is a strong guess. A guess that overrides proof is how the wrong two
legs get joined and one real transfer is reported twice.

A match is **one-to-one and both halves are consumed.** Without that, one deposit can be
claimed by two withdrawals of the same size -- an account that moves the same round number
twice a week produces exactly that, and the resulting link says the money went somewhere it
did not.

**Ambiguity is refused rather than broken by time.** When two candidates fit equally well,
neither is linked and both go to the queue. Picking the nearer one would be right most of the
time, and the times it is wrong are indistinguishable from the times it is right.

**The asymmetry is the point of the queue.** Bybit publishes the enums that let its
withdrawals be normalized; Binance does not (B2, F5). So an account moving coins from Binance
to Bybit produces a deposit with no withdrawal to match -- not because the matcher failed, but
because the outbound half was never ingestible. That leftover is recorded as a data-quality
finding (M7) rather than left silent, and its detail says which venue could not be read. A
matcher that quietly dropped it would make a documentation gap look like a balance.

**A link changes no number.** `transfer_links` joins two events and the fold ignores it
entirely: the deposit already added and the withdrawal already subtracted, on two different
integrations, and both are correct. What the link changes is the *interpretation* -- a
withdrawal read as a disposal invents a realized loss, and the link is what says it was not
one. So the exit test asserts that balances are byte-identical before and after matching, and
that the link exists. A milestone whose success is mostly the absence of change needs both
halves of that assertion, or a matcher that did nothing at all would pass (K49 learned this
the same way).

---

## Deliberately Out of Scope

| Not doing | Why |
|---|---|
| **Trade execution** | Ever. The API key permission will not allow it. Non-negotiable. |
| **Competing on integration count** | 1Token lists 72 exchanges and 4,224 DeFi protocols. That race is unwinnable and beside the point. We differentiate on **accuracy and risk depth**, not coverage. |
| Options, Greeks, VaR | Meaningless without options in scope. |
| Hedge mode (two-sided positions) | V1 is one-way mode only. |
| Multi-asset / portfolio margin | V2 at the earliest. |
| COIN-M futures | V2. |
| EVM wallets, Solana, DeFi, LP | A different ingestion domain entirely. |
| Tax-purpose FIFO/LIFO accounting | V2 — but the ledger stays lot-derivable from day one (K5). |
| CCXT as the exchange client | Its WebSocket support is paid, the Go port is transpiled rather than idiomatic, and user-data-stream normalization **is** this project's core domain work. Outsourcing it leaves CRUD behind. Its market-metadata and symbol-normalization model is still worth reading as reference. |
