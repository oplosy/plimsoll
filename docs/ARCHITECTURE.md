# Architecture

How the pieces fit, and why the boundaries fall where they do. Decisions are referenced
as `K<n>` (see `DECISIONS.md`); agent-facing invariants as `L<n>` (see `CLAUDE.md`).

---

## 1. Shape of the System

Two processes, one database, one cache. No message broker (K8).

```
                    ┌──────────────────────────────────────────┐
   browser ──TLS──▶ │  Caddy   (same origin, K27)              │
                    └───────┬──────────────────────┬───────────┘
                            │ /                    │ /api
                    ┌───────▼────────┐    ┌────────▼─────────┐
                    │  Next.js       │    │  cmd/api         │
                    │  dashboard     │    │  Huma + chi      │
                    └────────────────┘    └────────┬─────────┘
                                                   │ reads only
                                          ┌────────▼─────────┐        ┌──────────┐
                                          │   PostgreSQL     │◀──────▶│  Redis   │
                                          │  ledger + all    │        │ price hash│
                                          │  projections     │        │ SSE bus  │
                                          └────────▲─────────┘        └────▲─────┘
                                                   │ writes                 │
                                          ┌────────┴─────────┐              │
                                          │  cmd/worker      │──────────────┘
                                          │  ingest · value  │
                                          │  reconcile·alert │
                                          └────────┬─────────┘
                                                   │ REST + WS
                                           ┌────────▼─────────┐
                                           │  Binance + Bybit V5  │
                                           └──────────────────┘
```

**The split is a write/read split, and it is load-bearing.**

- `cmd/worker` is the only process that writes the ledger and advances projections.
  This is what makes the single-writer guarantee of K20 true rather than hoped for.
- `cmd/api` reads. It serves projections and streams; it never ingests.
- Because the API never writes the ledger, an API bug cannot corrupt history.

`cmd/api` may write exactly three things: sessions, user-authored configuration (alert
rules, strategies, manual transfer links), and integration records. None of these are
ledger data.

---

## 2. Tenancy

Two independent defences (K15). The first is expected to work; the second exists because
the first is enforced by discipline.

### Database roles

| Role | Used by | RLS |
|---|---|---|
| `plimsoll_owner` | `goose` migrations only | bypasses (table owner) |
| `plimsoll_app` | `cmd/api`, `cmd/worker` | enforced |

Table ownership bypasses RLS in Postgres. If the application connected as the owner —
the default in a naive setup — every policy in the schema would be decorative. The
separation is the whole mechanism, so it is verified by an integration test that asserts
`plimsoll_app` cannot read another account's rows.

`FORCE ROW LEVEL SECURITY` is set on every tenant table, which closes the remaining case
where a policy is skipped for the owner of a row.

### The transaction wrapper

`SET LOCAL` is scoped to a transaction, so every tenant-scoped read opens one:

```go
// internal/tenancy
func InTx(ctx context.Context, accountID uuid.UUID,
          fn func(*store.Queries) error) error
    // BEGIN
    // SET LOCAL app.account_id = $accountID
    // fn(queries)
    // COMMIT
```

No module reaches the pool directly. `tenancy.InTx` is the only path to tenant data, and
that is enforced by a lint rule, not by convention.

### Schema shape

Every tenant table carries `account_id` **on the row**, denormalized rather than reached
through a join, because an RLS policy that joins is both slow and easy to get subtly
wrong.

```sql
ALTER TABLE ledger_events
  ADD CONSTRAINT ledger_integration_belongs_to_account
  FOREIGN KEY (account_id, integration_id)
  REFERENCES integrations (account_id, id);
```

This composite foreign key makes attaching an event to another account's integration
impossible at the storage layer — not merely unlikely.

---

## 3. Ledger and Projections

### Identity, and why it excludes source

```
venue_event_id  =  <market>:<kind>:<symbol>:<venue id>
                   spot:trade:BTCUSDT:12345678
                   usdm:income:FUNDING_FEE:98765432
                   transfer:MAIN_UMFUTURE:11415955596
```

Constructed by the normalizer from exchange fields only — never from our own clock, our
own counters, or the ingest path. Two different ingest paths seeing the same exchange
event **must** produce the same `venue_event_id`. That property is what makes
`ON CONFLICT DO NOTHING` a correctness mechanism instead of a performance trick (K19,
L5).

`source` records which path saw it first. It is diagnostic.

### Ordering

```sql
ORDER BY event_time, venue_sequence, venue_event_id
```

Three levels, because the first two can tie (K21, L7):
- `event_time` — the exchange's own time; multiple fills of one order share it
- `venue_sequence` — the exchange's own monotonic id, which breaks that tie
- `venue_event_id` — a total order guarantee, so the result is deterministic even when
  an exchange reuses an id across markets

### The fold

```
                 ┌────────────────────────────────────────┐
ledger_events ──▶│  position.Apply(state, event) → state  │──▶ positions
  (ordered)      │      pure, no I/O, no clock (L4)       │   (projection)
                 └────────────────────────────────────────┘
```

`position.Apply` takes a state and an event and returns a state. It has no database
handle, no logger, and no access to time beyond what the event carries. This is why
scenario shocks (M7.5) cost almost nothing to add: replaying the fold against shocked
prices is the same function.

### Rebuild equality

The mandatory test (L3):

```
drop projection → fold entire ledger → compare, field by field, to the live table
```

If this test fails, the incremental path and the batch path disagree, which means one of
them has been quietly wrong for an unknown period. There is no "acceptable delta".

### Snapshots

`position_snapshots` accelerates `GET /portfolio?at=T` so that a historical query does
not fold the entire ledger from the beginning.

- **Cadence:** daily per integration, plus every 10,000 events
- **Invariant, and it is tested:** `snapshot(T) + events(T, T'] == full_fold(T')`

A snapshot is a cache. Deleting all snapshots must change performance and nothing else.

### Projection advance

Per integration, never global (K20, L6):

```
(last_event_time, last_venue_sequence, last_venue_event_id)   -- cursor, scoped to one integration
```

The worker holds an exclusive lease on the integration, so within that scope there is
exactly one writer and the cursor is safe. The global `seq` column never drives a cursor;
it exists for the lineage endpoint and for debugging.

---

## 4. Identity Resolution

The most expensive class of bug in this domain does not look like a bug. Quantities are
right, prices are right, and the holding is attached to the wrong instrument
(`COMPETITIVE-ANALYSIS.md` §3.1).

```
exchange payload
   symbol="BTCUSDT"  market=usdm  event_time=2024-03-11T09:00Z
        │
        ▼
instrument_aliases WHERE exchange='binance'
                     AND exchange_symbol='BTCUSDT'
                     AND market='usdm'
                     AND event_time <@ [valid_from, valid_to)   ◀── K22
        │
        ▼
instruments.id → BTC-USDT-PERP
```

Two things are deliberate here:

**Market is part of the lookup.** Spot `BTCUSDT` and perpetual `BTCUSDT` are the same
string on Binance and different instruments (K10). Omitting `market` merges two
positions into one and every downstream number silently drifts.

**The time window is part of the lookup.** Exchanges recycle symbols after delistings.
Resolving a 2024 event with the 2026 mapping produces a correct quantity attached to the
wrong asset — and because the number *looks* plausible, it gets debugged as a price
problem for weeks.

An overlapping-window exclusion constraint prevents two aliases from claiming the same
symbol at the same time, so this cannot be corrupted by a bad backfill.

A symbol that resolves to nothing does **not** get a guess. It raises a
`unknown_symbol` data-quality finding (K14) and the affected response is marked
`degraded` (K23).

---

## 5. Valuation

One run per response (K11, L10). Everything a total depends on is recorded with it.

```
valuation_runs
  id, as_of, numeraire='USD', price_source, assumed_peg, freshness
valuation_prices                    -- the audit trail
  run_id, asset_id, price_usd, path, source, observed_at
```

### The price path

There is no USDT/USD market, so USD valuation walks a graph (K17):

```
BTC   ──BTCUSDT──▶ USDT ──USDC/USDT──▶ USDC ──assumed──▶ USD
```

Each hop is stored. `GET /positions/{id}/lineage` can therefore answer not only "which
events produced this position" but "which prices produced this number, from which
source, observed when".

When a leg falls back to a hard-coded 1.00, `assumed_peg` is set and the response is
`degraded`. The system never claims certainty it does not have — during a depeg that
flag is the single most valuable field in the API.

### Freshness

Replaces the boolean `stale` (K23, L11). It is API surface, not diagnostics:

```json
{
  "as_of": "2026-09-01T12:00:00Z",
  "freshness": {
    "status": "degraded",
    "reasons": [
      { "code": "backfill_incomplete", "severity": "warn",
        "detail": "binance spot: 2019-01..2021-06 pending", "since": "..." },
      { "code": "assumed_peg", "severity": "info",
        "detail": "USDC→USD assumed 1.00", "since": "..." }
    ]
  }
}
```

| code | raised by |
|---|---|
| `ws_gap` | missed keepalive, disconnect, sequence hole |
| `price_stale` | the oldest leg of the response's valuation run is older than tolerance, measured from the read time (K47) |
| `backfill_incomplete` | staged backfill still running (K26) |
| `history_truncated` | the venue will not return data before a cutoff -- permanent, unlike `backfill_incomplete` |
| `assumed_peg` | a price path fell back to 1.00 (K17) |
| `unknown_symbol` | alias resolution failed (K22) |
| `reconciliation_mismatch` | an open finding above tolerance |
| `fee_price_missing` | no price for a fee asset at `event_time` (K18) |
| `valuation_unavailable` | no valuation run has completed, so the response carries subtotals per quote asset and no total. **Narrowed in M4, not retired** (K47): a fresh install whose feed has never connected must not report a confident zero |
| `ingest_stalled` | no worker is reading an integration, or the one that was has stopped reporting (K39) |
| `projection_lagging` | events are in the ledger that the fold has not reached (K38) |
| `lineage_mismatch` | replaying a position's events did not reproduce the stored projection (K43) |
| `negative_balance` | the ledger implies holding less than nothing of an asset (K14, K44) |
| `collateral_stale` | the captured margin picture is older than the API's tolerance: served, because an ageing liquidation distance is still the best answer there is (M5) |
| `collateral_unavailable` | no capture exists for an active integration, so the margin buffer is unknown -- and the response omits the integration rather than rendering it at zero, because unknown and zero are opposite claims (M5) |

`status` is the worst severity present. A caller that reads nothing but `status` is
still safe, which is the point.

---

## 6. Worker Model

Multi-user turns ingestion into a scheduling problem the original single-user draft did
not have.

### Ownership by lease

```sql
integration_leases (
  integration_id  UUID PRIMARY KEY,
  owner_id        UUID NOT NULL,      -- worker instance
  heartbeat_at    TIMESTAMPTZ,
  expires_at      TIMESTAMPTZ
);
```

Claim: `INSERT … ON CONFLICT (integration_id) DO UPDATE … WHERE expires_at < now()`.
A single atomic statement, so two workers cannot both win.

V1 runs one worker process. The lease still exists from day one because it is what makes
K20's single-writer guarantee real rather than an assumption about deployment. Adding a
second worker later becomes starting a process, not a redesign — and, just as
importantly, accidentally starting a second copy today is *safe* instead of silently
double-ingesting.

### Per-integration supervisor

Each owned integration gets one supervisor goroutine:

```
supervisor(integration)
  ├── listenKey lifecycle (obtain, keepalive, rotate)
  ├── WS user data stream ──▶ normalize ──▶ ledger insert ──▶ projection advance
  ├── gap detector ──▶ triggers a bounded REST resync for the affected window
  └── scheduled work: reconciliation, backfill chunks
```

The supervisor is a state machine, not a loop with retries scattered through it. Its
states — `connecting`, `live`, `degraded`, `resyncing`, `backfilling` — map directly onto
the freshness reasons in §5, so the ingest state and what the user is told about it
cannot drift apart.

### Two-tier rate limiting

```
        ┌──────────────────────────┐
        │  per-integration budget  │   weight per API key
        └────────────┬─────────────┘
                     ▼
        ┌──────────────────────────┐
        │  shared per-IP budget    │   every outbound call, all users
        └────────────┬─────────────┘
                     ▼
                  Binance
```

Binance limits weight per API key **and** separately per IP, and caps WS connections per
IP (K24). With every user behind one VPS address, per-key accounting alone reports
healthy budgets right up until the IP is banned and every user goes dark at once. Both
tiers are mandatory; the second is a process-wide singleton.

WS connections are handed out by a quota-aware manager for the same reason.

### Work priority

```
realtime  >  reconciliation  >  backfill
```

Fixed, not runtime-tunable. Backfill is chunked with persisted progress (K26) and
interleaved, so a new user importing seven years of history cannot delay another user's
liquidation-distance update. Under contention, backfill starves — deliberately, because
it is the only one of the three that is not time-critical.

---

## 7. Ingestion Flows

### Backfill (staged, resumable)

```
create integration → verify key is read-only (K9) → discover instruments
   → spot:    per-symbol trade history
              (Binance requires a symbol; derive the candidate set from
               balances + order history)
   → futures: userTrades + income history (funding, commission, realized pnl)
   → deposits / withdrawals
   → idempotent ledger insert (K19) → advance projection
   → persist chunk progress; portfolio stays `backfill_incomplete` until done
```

Progress is persisted per chunk, so an interrupted backfill resumes rather than
restarting. A restart that begins from zero on a seven-year account is how rate limits
get exhausted and the import never finishes.

### Realtime

```
listenKey → user data stream → keepalive
   → event → normalize → ledger insert → projection advance
   → valuation invalidate → SSE push
```

### Gap handling

This is the flow that decides whether the product is trustworthy.

On a disconnect, a missed keepalive, or a sequence hole:

1. The position is **no longer considered live**
2. A bounded REST resync is triggered for the affected window
3. `ws_gap` enters `freshness`; responses are served `degraded` (K23)
4. Once the resync lands and reconciles, the flag clears

The system never guesses across a gap, and it never hides one. Serving a confident wrong
number is the single worst outcome available to this product — worse than an error,
because an error gets investigated and a wrong number gets acted on.

### Market data → risk

Prices move without trades, so risk cannot be trade-driven:

```
mark price WS → Redis latest (K28) + price_ticks minutely (K7)
   → valuation run → equity, unrealized PnL, exposure, liquidation distance
   → threshold evaluation → alert
```

Alerts evaluate **on completed valuation runs**, not per tick. Per-tick evaluation would
fire on prices that never entered a published total, alerting on numbers the user was
never shown.

---

## 8. Reconciliation and Data Quality

Two different questions, deliberately two modules.

**`reconciliation`** — *does our state match the exchange's?*
Every 5 minutes: pull a REST snapshot (balances, `positionRisk`), compare against our
projection, record findings outside tolerance.

Tolerance is **per metric**, not one global epsilon: quantity tolerance is dust-scale,
USD-value tolerance is basis-point-scale, and comparing them with one number produces
either constant noise or silent blindness.

V1 policy is **detect and report; never auto-correct**. Automatic resync arrives in V2,
once finding classification (`missing_event` / `duplicate` / `rounding` /
`unsupported_event`) has been validated against real data. Auto-correcting a
misclassified finding writes a wrong correction into an append-only ledger, which cannot
be undone.

**`quality`** — *is our state internally coherent?*
Runs against our data alone, needing no exchange call. The checks are listed in K14. The
strongest signal is **negative balance**: if the ledger implies selling more than was
ever held, an event is missing — and the check finds it without knowing what the missing
event was.

Both feed `GET /data-quality`, which is a first-class product surface. "Show me why your
numbers are right" is the pitch (`COMPETITIVE-ANALYSIS.md` §7); this endpoint is the
answer.

---

## 9. Risk

All metrics are pure functions of `(positions, prices, exchange snapshot)` — which is
what makes scenario shock nearly free (M7.5).

```
equity              = cash + spot value + perp unrealized PnL
gross_exposure      = Σ |notional|
net_exposure        = Σ  notional
leverage            = gross_exposure / equity
concentration       = |asset_exposure| / gross_exposure
unrealized_pnl      = Σ (mark − avg_entry) × qty × direction
realized_pnl        = cumulative from ledger, including funding and fees (K18)
drawdown            = decline from peak equity  (needs equity_snapshots)
liq_distance        = |mark − liq_price| / mark  (liq_price from exchange, K6)
margin_utilization  = used margin / available margin
funding_cost        = periodic funding total
```

### Strategy level is not optional

Computed per `strategy_id` as well as per portfolio (K13). For a delta-neutral basis
trade:

```
BTC spot long   +$50,000
BTC perp short  −$50,000

naive:   gross $100k / equity $50k  →  "2× leveraged, risky"     ← wrong
correct: net BTC delta ≈ 0          →  no directional risk
         real risks: funding flip, short-leg liquidation, basis, venue
```

A system that cannot group these two legs alerts the user constantly and incorrectly,
and a user who learns to ignore alerts has no alerting. `net_delta_per_asset` is
reported within the strategy.

---

## 10. API Contract

Rules that hold for every endpoint, without exception:

1. All numbers are JSON **strings** (L1). A `float64` in a client parser is our bug too.
2. Every response carries `as_of` and `freshness` (L10, L11).
3. Every total in one response comes from one `valuation_run` (K11).
4. Ledger pagination is cursor-based on the canonical order `(event_time, venue_sequence,
   venue_event_id)`, never on `seq` -- identity values are assigned before commit, so a
   `seq` cursor can skip a row that was still in flight (K20, K41). While a backfill is
   running the page is a view of an incomplete set, and the response says so.
5. Mutations are limited to configuration and integrations. **No endpoint writes ledger
   events**, and none ever places an order (L13).
6. `GET /positions/{id}/lineage` opens any number down to the events and prices that
   produced it. It is the product thesis in endpoint form, and it is nearly free because
   of K1.

---

## 11. Schema Deltas from the Original Draft

Changes forced by the review and the multi-user decision. Each one is cheap now and
requires rebuilding the ledger later.

| Change | Reason |
|---|---|
| `account_id` on every tenant table + composite FK | K15 — RLS needs a column, not a join |
| `venue_event_id`, unique with `integration_id`; `source` demoted to metadata | K19 — the old key never deduplicated |
| `venue_sequence` column, required | K21 — `event_time` ties are the norm, not the exception |
| `valid_from` / `valid_to` + exclusion constraint on both alias tables | K22 — symbols get recycled |
| `market` included in instrument alias lookup | K10 — spot and perp share the symbol string |
| `key_version` + per-account wrapped DEK on credentials | K25 — bounded blast radius, per-account rotation |
| `integration_leases` table | K20 — makes single-writer real |
| `valuation_runs.numeraire`, `assumed_peg`; new `valuation_prices` | K17 — the price path is auditable |
| `stale BOOLEAN` → structured `freshness` | K23 — one bit cannot carry seven conditions |
| `backfill_progress` per integration and chunk | K26 — resumable, never restarted |
| Fee is a column on its parent event only; `FEE` type reserved for orphan fees | K18 — the draft allowed double counting |
| `accounts`, `sessions`, `invites` | K16 |

---

## 12. Testing Architecture

The engine/infrastructure split is enforced by the test tooling, not just intended.

| Layer | Runs on | Speed | Covers |
|---|---|---|---|
| **Unit** | no Docker, no DB | milliseconds | pure engines: fold, position, valuation, pnl, risk |
| **Integration** (`//go:build integration`) | real Postgres via compose | seconds | RLS, `NUMERIC(38,18)` behaviour, exclusion constraints, lease contention |
| **Fixture replay** | no Docker | milliseconds | recorded payloads → expected state, compared with `go-cmp` |

Integration tests need a **real** Postgres. RLS behaviour, `NUMERIC(38,18)` rounding, and
`EXCLUDE USING gist` cannot be tested against a mock or SQLite — a green test on a fake
database would be worse than no test, because it would be believed.

### The four invariant tests

These are not feature tests. Each one guards a property the whole system rests on:

1. **Idempotency** — applying the same event set twice leaves the state unchanged (K3/K19)
2. **Order independence** — events sorted canonically produce identical state regardless
   of ingest order (K21)
3. **Rebuild equality** — the `positions` table equals a fold from zero, exactly (L3)
4. **Tenant isolation** — `plimsoll_app` cannot read another account's rows, including
   when the application-level `WHERE` is deliberately omitted (K15)

Test four is written by deliberately removing the primary defence, to prove the backstop
holds on its own. A tenant-isolation test that passes only because both layers are
present has verified nothing about either.

### Fixtures

Recorded from real accounts into `testdata/fixtures/binance/`. Account ids, `listenKey`
values and API keys are stripped by a redaction script — a checked-in program, not a
manual step, because a manual step will eventually be skipped and a credential will land
in git history where it lives forever.
