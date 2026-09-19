<!--
  ============================================================================
  THIS FILE IS DUPLICATED. CLAUDE.md and AGENTS.md MUST be byte-identical.
  Edit one, copy to the other, verify with:  diff CLAUDE.md AGENTS.md
  A change that lands in only one of them is a defect, not a detail.
  ============================================================================
-->

# Plimsoll — Agent Operating Manual

Crypto portfolio & risk infrastructure for a leveraged CEX trader: a canonical,
reconciled, strategy-aware realtime risk engine built on an append-only ledger.

The product is not "we show your balance". The product is **the numbers are right,
and we can prove it** — down to the events that produced them. Every design choice
below serves that claim.

**Read before writing code:**

| File | When |
|---|---|
| `docs/DECISIONS.md` | Always. The decision register (K1–K57). Why the system is shaped this way. |
| `docs/ARCHITECTURE.md` | Before touching any module. Boundaries, data flow, tenancy, worker model. |
| `docs/PROJECT.md` | Scope, canonical model, API surface, milestones. |
| `docs/COMPETITIVE-ANALYSIS.md` | Positioning, and the failure modes competitors hit that we must not. |

---

## 1. The Laws

These are invariants, not preferences. Each one exists because violating it silently
corrupts financial data. If a task seems to require breaking one, the task is wrong —
stop and raise it.

**L1 — Money is never `float64`.**
Postgres `NUMERIC(38,18)`, Go `shopspring/decimal`, JSON **string**. No exceptions:
not in logs, not in test helpers, not in "just a quick calculation". A `float64` in
any price / quantity / fee / balance path is a bug even when the test passes.

**L2 — The ledger is append-only.**
`ledger_events` rows are never `UPDATE`d and never `DELETE`d. Corrections are new
events. If you find yourself writing an UPDATE against the ledger, you have a design
error — stop and ask.

**L3 — Everything else is a projection.**
`positions`, portfolio, PnL, exposure, risk are a pure fold over the ledger. Every
projection table must survive being dropped and rebuilt from events, producing a
byte-identical result. That rebuild-equality test is mandatory and never skipped.

**L4 — Engines are pure functions.**
No DB handle, no clock, no network, no logger inside `ledger`, `position`,
`valuation`, `pnl`, `risk`. Time and prices are **inputs**. If an engine wants
`time.Now()`, pass the time in. This is what makes scenario shocks and historical
reconstruction cheap, and it is what lets the engine be tested without Docker.

**L5 — Event identity excludes the source.** *(K19)*
Dedup key is `UNIQUE (integration_id, venue_event_id)`, e.g.
`spot:trade:BTCUSDT:12345678`. REST backfill and the WS stream report the **same**
trade; if `source` were part of the key they would both insert and the position would
double. `source` is metadata — "who saw it first" — never identity.

**L6 — Never advance a projection on the global `seq`.** *(K20)*
`BIGSERIAL` values are assigned *before* commit. Under concurrent inserts a reader can
observe `seq=100` committed while `seq=99` is still in flight; a `last_seq` cursor
skips it permanently and the ledger is append-only, so it never comes back. Projections
advance **per `integration_id`**, protected by the single-writer lease. `seq` exists for
debugging and lineage only.

**L7 — Canonical event order is `(event_time, venue_sequence, venue_event_id)`.** *(K21)*
Never `event_time` alone. Exchange fills of one order share a millisecond timestamp;
ordering by time alone is undefined and makes `avg_entry_price` depend on ingest order.

**L8 — An exchange symbol is never a key, and never resolved with today's mapping.** *(K10, K22)*
Always go through the alias table, **as of the event's `event_time`**. Exchanges recycle
symbols after delisting. Resolving a 2024 trade with the 2026 mapping is how a correct
quantity ends up attached to the wrong instrument — the industry's #1 silent corruption.

**L9 — A fee belongs to the event that caused it.** *(K18, A5)*
Fees ride on the `fee` / `fee_asset` columns of their parent event. The standalone `FEE`
event type is only for fees with no parent. Never both, or it is counted twice.
Fees are never folded into `avg_entry_price`.

**L10 — One valuation run per response.** *(K11)*
Every portfolio / risk response is produced from a single `valuation_run` and carries
`as_of`, the price source, and the price path used. Two different price sources
producing two different totals inside one response is forbidden. This is the
"every screen shows a different total" failure mode, and it is designed out.

**L11 — Never silently serve data you doubt.** *(K23)*
WS gap, stale price, incomplete backfill, assumed stablecoin peg, unresolved symbol,
open reconciliation finding — each one appears in the response's structured `freshness`
object with a reason and a severity. Degraded and visible always beats confident and
wrong. Silence is the worst possible failure.

**L12 — Every tenant query carries `account_id`.** *(K15)*
Application-level scoping is the primary defence; Postgres RLS is the backstop, not the
plan. The application connects with a restricted role — never the table owner, because
owners bypass RLS. All tenant tables use `FORCE ROW LEVEL SECURITY`.

**L13 — Credentials are read-only, encrypted, and never printed.** *(K9, K25)*
API keys are verified read-only at connection time; an over-permissioned key is rejected.
Secrets are envelope-encrypted (per-account DEK wrapped by the master KEK) and never
appear in logs, traces, error messages, or fixtures. **Trade execution is never
implemented** — not behind a flag, not in a comment, not "for later".

**L14 — Redis stores nothing that cannot be rebuilt.** *(K28)*
Losing Redis degrades latency. It never loses data. Anything durable lives in Postgres.

**L15 — Raw payloads are kept forever.**
Every ledger event stores its exchange payload in `raw` JSONB. When a normalization bug
surfaces months later, replaying the ledger from raw is the thing that saves the project.

---

## 2. Working Agreement

### Roles

| Role | Who | Does | Does not |
|---|---|---|---|
| **Implementer** | Claude (you) | Design, schema, migrations, production code, **and the tests** — written test-first | Skip the failing-test step |
| **Reviewer** | Codex (run by the human) | Independent second-eye review of completed work: correctness, invariant violations, missed edge cases | Write production code |

You write the tests. This means your tests share blind spots with your implementation —
so Codex review is not a formality, it is the compensating control. When review feedback
arrives, verify the claim against the code before agreeing or disagreeing. Do not
perform agreement, and do not implement a suggestion you believe is wrong without
saying so.

### Test-driven, always

RED → GREEN → REFACTOR. Write the failing test first, watch it fail for the right
reason, then implement. A test written after the code has already passed proves the
code runs; it does not prove the code is correct.

### Session discipline

- **One module per session.** Cross-cutting refactors get their own session.
- **Fixtures before network.** Record real exchange payloads into
  `testdata/fixtures/binance/` (redacted), then develop against the fixture. Never
  iterate against the live API.
- **Verify venue details against the official docs** — symbol requirements, `listenKey`
  lifetime, `positionRisk` version, weight costs, rate-limit headers, a signature payload,
  a status enum. Never from memory, never from a blog post. Getting this wrong produces
  plausible, wrong numbers. Findings are numbered and dated: **F1–F21** for Binance,
  **B1–B5** for Bybit, and a fact that could not be verified is written down as unverified
  rather than guessed at.
- **Milestone M1 ships before any network code.** Debugging the engine against live data
  is the most expensive path available.

---

## 3. Definition of Done

A change is not done until all of these hold. State the evidence; do not assert success
without having run the command.

- [ ] A test was written first and observed failing for the right reason
- [ ] `make test` green (unit — no Docker, engines pure)
- [ ] `make test-integration` green when schema, queries, or RLS changed
- [ ] `make lint` clean
- [ ] `make generate` produces no diff (sqlc output is committed and current)
- [ ] Rebuild-equality test passes when any projection changed (L3)
- [ ] No `float64` introduced on a money path (L1)
- [ ] New tenant tables have `account_id`, RLS enabled and forced (L12)
- [ ] `diff CLAUDE.md AGENTS.md` is empty
- [ ] Ready to hand to Codex for review

If a step was skipped, say so. If a test fails, report the output. Never round a partial
result up to "done".

---

## 4. Commands

```
make generate           # sqlc — regenerate typed queries; output is committed
make migrate            # goose up against a local checkout (owner role, never the app role)
make test               # unit: pure engines, no Docker, fast
make test-integration   # //go:build integration — real Postgres via compose
make lint               # golangci-lint
make frontend-check     # dashboard: types, the no-float-parsing guard (L1), money tests
make docs-check         # CLAUDE.md ≡ AGENTS.md
make up / make down     # docker compose; `up` migrates first and waits for it (K46)
```

`make test` must stay fast enough to run on every save. If it starts needing Docker,
an engine has grown a dependency it should not have (L4).

**`make migrate` is for a checkout, not for the stack.** Inside compose the schema is
applied by a one-shot `migrate` service running `plimsollctl migrate`, which carries the
migrations embedded in its own binary — so the image and the schema it applies cannot be
different revisions of each other, and the api and the worker cannot start against a
database that has not been migrated (K46). Running `make up` is enough; running
`make migrate` afterwards is a no-op.

The operator CLI is the only thing that connects as `plimsoll_owner`:

```
plimsollctl migrate                          # apply every pending migration
plimsollctl invite -email <addr> [-ttl 168h] # mint a single-use invite      (K16)
plimsollctl record ...                       # capture a redacted fixture
```

---

## 5. Code Conventions

- **English everywhere** — identifiers, comments, commit messages, API field names,
  error strings, docs.
- `context.Context` is the first parameter of anything that does I/O.
- Errors are wrapped with `%w` and carry the identifiers needed to debug
  (integration id, instrument, event id) — **never the credential**.
- No `panic` outside `main`. No global mutable state.
- Table-driven tests. Golden fixtures compared with `go-cmp`.
- A file past ~400 lines is a signal it is doing too much. Split on responsibility,
  not on line count.
- Public types and exported functions get a doc comment stating what it does, how it is
  used, and what it depends on. If you cannot state that in two lines, the boundary is
  wrong.
- Numbers cross the API as strings. Every response includes `as_of` and `freshness`.

---

## 6. Repository Layout

**Written.** This half is the map of what exists; if a module is here, read it before
writing something that overlaps it.

```
CLAUDE.md · AGENTS.md      identical; this file
docs/
  DECISIONS.md             K1–K57 decision register
  ARCHITECTURE.md          module boundaries, data flow, tenancy, worker model
  PROJECT.md               scope, canonical model, API, milestones
  COMPETITIVE-ANALYSIS.md  positioning and competitor failure modes
  BINANCE-API-NOTES.md     F1-F21, every venue fact verified against the docs
  BYBIT-API-NOTES.md       B1-B5, the same for the second venue
  plans/                   per-milestone implementation plans
backend/
  cmd/api/                 HTTP process — reads only, never writes the ledger
  cmd/worker/              the sole ledger writer; one supervisor per integration
  cmd/plimsollctl/         operator CLI: migrate · invite · record   (owner role only)
  migrations/              goose SQL + the embed the CLI ships with       (K46)
  internal/
    auth/          sessions, argon2id, invite-based account creation      (K16)
    tenancy/       account scoping + the RLS transaction wrapper          (K15)
    crypto/        envelope encryption: per-account DEK behind a KEK      (K25)
    integration/   exchange connections and their stored credentials      (K25)
    exchange/binance/   rest · ws · normalizer · fixtures
    exchange/bybit/     rest · normalizer · fixtures; transfers only, by decision (M8)
    ratelimit/     two-tier: per-integration weight + shared per-IP       (K24)
    backfill/      resumable per-scope history walk                       (K26, K33)
    asset/         canonical asset registry, time-scoped alias resolution (K10, K22)
    instrument/    the same, for tradeable pairs                          (K10)
    ledger/        append-only writes + the ordered read                  (L2, L7)
    position/      the average-cost fold — pure                           (K5, L4)
    balance/       the asset fold — pure                                  (K44, L4)
    projection/    the I/O around both folds + rebuild                    (L3, K38)
    ingest/        the state vocabulary the worker publishes and API reads (K39)
    events/        LISTEN/NOTIFY fan-out behind the SSE streams             (K51)
    worker/        supervisor, lease, stream adapters                     (K20, K37)
    portfolio/     the read model: holdings, totals, history at T, pnl (K43, K47, K48)
    marketdata/    price ingest: REST snapshot, live stream, minute ticks   (K7, M4)
    valuation/     one run, USD numeraire, auditable price paths        (K11, K17, M4)
    collateral/    the margin picture: capture, buffer, MMR tiers      (K6, K50, M5)
    freshness/     reason codes, severities, the response envelope        (K23, L11)
    strategy/      the sleeve tag, and the rebuild that must not erase it   (K13, K30)
    risk/          exposure, leverage, concentration, net delta -- pure   (K13, L4)
    scenario/      the price shock: equity and buffer after a move -- pure (K56, M7.5)
    quality/       the findings register + the checks needing no venue call (K14, K53)
    reconciliation/  our fold vs the venue's answer, and the classifier   (K54, K55, M7)
    transfer/      cross-venue matching: two halves, one movement      (K12, K57, M8)
    alert/         hysteresis, cooldown, delivery, the record              (M6, L13)
    httpapi/       routing, session cookie, handlers                      (K16, K27)
    obs/           slog with secret redaction + OTel                      (L13)
    store/         sqlc output and the pool constructor
  testdata/fixtures/binance/   recorded, redacted real payloads
  testdata/fixtures/bybit/     the same; `documented` until a key exists
frontend/          Next.js dashboard: portfolio, risk, exposure, alerts     (K27)
deploy/            compose topology, Caddyfile, postgres init
```

**Planned.** Empty as of M8: every boundary this file ever listed as coming has been written.
A directory is created when its module is, never in advance — so when the next one is decided
it is named here first, and work that belongs in it is not quietly absorbed by a neighbour.

`exchange/bybit/` is deliberately partial, and the line is drawn rather than drifted to: it
covers deposits and withdrawals, which is what cross-venue matching needs, and **not** trades,
positions or funding. A Bybit portfolio is a later milestone. Everything it does encode is
verified in `docs/BYBIT-API-NOTES.md` (B1–B5) and dated; nothing is remembered.

---

## 7. Hard "Never" List

- Never implement trade execution, or request an API key permission that allows it.
- Never `UPDATE` or `DELETE` a `ledger_events` row.
- Never put a raw exchange symbol in a foreign key or a projection key.
- Never resolve an alias with the current mapping when the event has an `event_time`.
- Never let a plaintext credential reach a log, trace, error, fixture, or test output.
- Never return a total without `as_of` and `freshness`.
- Never assume a stablecoin is worth exactly 1.00 without flagging `assumed_peg`.
- Never connect the application to Postgres as the table owner.
- Never claim a test passes without having run it.
