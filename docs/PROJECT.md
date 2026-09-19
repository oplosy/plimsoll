# Plimsoll — Project Specification

Backend-focused position/risk engine with an ingestion and reconciliation layer.
The UI is secondary; the product is correct portfolio state derived from a ledger.

**Positioning:** for the leveraged CEX trader — a canonical, reconciled,
strategy-aware realtime risk engine built on an append-only ledger. Retail trackers show
balances, tax tools keep history correct, institutional PMS platforms do both but are
out of reach. Full reasoning: `COMPETITIVE-ANALYSIS.md`.

**The axis we will not compete on: integration count.** Differentiation is not coverage,
it is accuracy and risk depth. The most visible feature of this product is not how many
exchanges it supports — it is reconciliation status and the traceability of every number
down to its events.

**Companion documents**

| Document | Contains |
|---|---|
| `DECISIONS.md` | K1–K57: every architectural decision, its rationale and its cost |
| `ARCHITECTURE.md` | Module boundaries, data flow, tenancy mechanics, worker model, schema deltas |
| `COMPETITIVE-ANALYSIS.md` | Market segmentation, the gap, competitor failure modes |
| `../CLAUDE.md` = `../AGENTS.md` | Agent operating manual: invariants, workflow, definition of done |

This file states **what** is being built. `DECISIONS.md` states **why**.
`ARCHITECTURE.md` states **how the pieces fit**. Content is not duplicated between them.

---

## 1. V1 Scope (locked)

**In**

- Binance (spot + USD-M perpetual), read-only API key
- Historical backfill + realtime user data stream
- Canonical ledger, position engine, PnL, portfolio, exposure/leverage
- Market data ingest, price history, mark-to-market
- Reconciliation (our state ↔ exchange snapshot)
- Risk thresholds and alerting
- Realtime dashboard over SSE
- **Multi-user**: invite-provisioned accounts, password sessions, tenant isolation (K15, K16)
- **USD numeraire** with priced stablecoins (K17)

**Out (V1)**

- Bybit / Coinbase — V2, and the real test of the normalization layer
- EVM wallets, Solana, DeFi, LP positions
- COIN-M futures, options
- Hedge mode (two-sided positions)
- FIFO/LIFO tax accounting — but the ledger stays lot-derivable from day one (K5)
- Multi-asset / portfolio margin
- **Trade execution — never.** The API key permission will not allow it.

---

## 2. Canonical Model

### Asset and Instrument

An exchange symbol is not a canonical instrument, and an instrument is not an asset (K10).
Two layers:

```
assets
  id
  canonical_symbol      BTC / ETH / USDT
  kind                  native | token | stablecoin | fiat
  chain                 ethereum          (tokens)
  contract_address      0x...             (tokens)
  is_wrapped            WBTC → underlying_asset_id = BTC

asset_aliases
  source                binance | bybit | coingecko
  external_symbol       BTC
  asset_id              FK
  valid_from, valid_to  ◀── K22, time-scoped resolution

instruments
  id
  canonical_symbol      BTC-USDT-PERP / BTC-USDT-SPOT
  kind                  spot | perp
  base_asset, quote_asset, settle_asset
  contract_size         NUMERIC

instrument_aliases
  exchange              binance
  exchange_symbol       BTCUSDT
  market                spot | usdm      ◀── part of the key, not a detail
  instrument_id         FK
  valid_from, valid_to  ◀── K22
```

On Binance, spot `BTCUSDT` and perpetual `BTCUSDT` are the same string and different
instruments. Without this split, positions merge and everything downstream is wrong.
Get it right on day one — retrofitting means rebuilding the ledger.

### Ledger event types

```
TRADE                buy / sell
TRANSFER             between accounts (spot ↔ futures)
DEPOSIT
WITHDRAWAL
FUNDING_PAYMENT      perp funding — affects realized PnL
FEE                  only when it has no parent event (K18)
COMMISSION_REBATE
LIQUIDATION          a special case of trade, flagged separately
POSITION_ADJUSTMENT  ADL, settlement, etc.
```

### Schema (summary)

```sql
accounts   (id, email, password_hash, created_at, disabled_at);
sessions   (token_hash, account_id, created_at, expires_at, last_seen_at);
invites    (token_hash, created_by, consumed_by, expires_at);

integrations (
  id, account_id, exchange, label,
  credential_ciphertext BYTEA,    -- envelope-encrypted (K25)
  wrapped_dek           BYTEA,
  key_version           INT,
  status, created_at,
  UNIQUE (account_id, id)         -- target of the composite FK below
);

ledger_events (
  seq             BIGSERIAL PRIMARY KEY,   -- lineage/debug only, NEVER a cursor (K20)
  account_id      UUID NOT NULL,           -- denormalized for RLS (K15)
  integration_id  UUID NOT NULL,
  venue_event_id  TEXT NOT NULL,           -- source-independent identity (K19)
  venue_sequence  BIGINT,                  -- exchange's own id, ordering tiebreak (K21)
  source          TEXT NOT NULL,           -- metadata only: who saw it first
  event_type      TEXT NOT NULL,
  instrument_id   BIGINT,
  strategy_id     UUID,                    -- K13
  side            TEXT,                    -- buy | sell
  quantity        NUMERIC(38,18),
  price           NUMERIC(38,18),
  fee             NUMERIC(38,18),          -- belongs to THIS event only (K18)
  fee_asset       TEXT,
  asset_id        BIGINT,                  -- what moved, when no pair is traded (00012)
  transfer_from   TEXT,                    -- TRANSFER only: spot|usdm|coinm|margin|
  transfer_to     TEXT,                    --   funding|external. Both, or neither (K49)
  event_time      TIMESTAMPTZ NOT NULL,    -- exchange time; drives all calculation (K2)
  ingested_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  raw             JSONB NOT NULL,          -- mandatory; the project's insurance policy
  UNIQUE (integration_id, venue_event_id), -- K19
  FOREIGN KEY (account_id, integration_id)
    REFERENCES integrations (account_id, id)
);
CREATE INDEX ON ledger_events (integration_id, event_time, venue_sequence);

positions (                       -- projection: droppable and rebuildable (L3)
  account_id, integration_id, instrument_id,
  quantity, avg_entry_price, realized_pnl,
  last_event_time, last_venue_sequence, last_venue_event_id,   -- the whole order (L7)
  updated_at,
  PRIMARY KEY (integration_id, instrument_id)
);
-- The cursor is all three columns, not last_venue_sequence alone: two events can share a
-- sequence, and a sequence-only cursor rejects the second one and loses it for good.

position_fees (                   -- per asset and unconverted (K18, L9)
  account_id, integration_id, instrument_id, fee_asset, amount
);

projection_cursors (              -- how far the fold got, per integration (K20, L6)
  account_id, integration_id, last_event_time, last_venue_sequence, last_venue_event_id
);

position_strategies (             -- K30: user input, so NOT on the projection above
  account_id, integration_id, instrument_id, strategy_id, assigned_at
);

position_snapshots  (account_id, integration_id, as_of, venue_sequence, state JSONB);
price_ticks         (instrument_id, ts, mark_price, index_price);   -- BRIN on ts
equity_snapshots    (account_id, ts, equity_usd);                   -- drawdown input

integration_leases  (integration_id PK, owner_id, heartbeat_at, expires_at);  -- K20
backfill_progress   (integration_id, scope, cursor, completed_at);            -- K26

valuation_runs      (id, account_id, as_of, numeraire, price_source,
                     assumed_peg, freshness JSONB);                 -- K11, K17, K23
valuation_prices    (run_id, asset_id, price_usd, path JSONB, source, observed_at);

exchange_snapshots  (account_id, integration_id, taken_at, kind, payload JSONB);
reconciliation_runs     (id, account_id, integration_id, started_at, status);
reconciliation_findings (run_id, instrument_id, ours, theirs, delta, severity, class);

alert_rules (account_id, integration_id, metric, operator, threshold,
             enabled, cooldown_seconds, hysteresis_pct);
alerts      (rule_id, fired_at, cleared_at, snapshot JSONB);

strategies  (id, account_id, name, kind);   -- basis_trade | directional | hedge

transfer_links (                            -- K12
  id, account_id,
  out_event_seq, in_event_seq,
  match_method,          -- txid | heuristic | manual
  confidence, confirmed_by_user
);

data_quality_findings (account_id, integration_id, detected_at, check_name,
                       severity, instrument_id, details JSONB, resolved_at);  -- K14
fx_rates              (base_asset, quote_ccy, ts, rate);
```

Every tenant table carries `account_id`, has RLS enabled and forced, and is only reached
through `tenancy.InTx` (K15).

**Storing the raw payload is mandatory.** When a normalization bug surfaces, rebuilding
the ledger from `raw` is what saves the project.

---

## 3. Critical Flows

Detailed in `ARCHITECTURE.md` §6–§8. Summary:

| Flow | Shape | Key property |
|---|---|---|
| **Backfill** | verify key → discover instruments → spot trades, futures trades + income, deposits/withdrawals → idempotent insert → rebuild | Chunked and resumable; `backfill_incomplete` in `freshness` until done (K26) |
| **Realtime** | listenKey → user stream → normalize → insert → project → SSE | Single writer per integration, held by lease (K20) |
| **Gap handling** | disconnect / missed keepalive / sequence hole → position not live → bounded REST resync → `ws_gap` in `freshness` | Never guess across a gap, never hide one |
| **Market data → risk** | mark price WS → Redis latest + minutely `price_ticks` → valuation → thresholds → alert | Alerts evaluate on completed valuation runs, not per tick |
| **Reconciliation** | every 5 min: REST snapshot → compare → classified findings | V1: detect and report. **No auto-correction** until classification is validated |

---

## 4. Risk Metrics (V1)

```
equity              = cash + spot value + perp unrealized PnL      (in USD, K17)
gross_exposure      = Σ |notional|
net_exposure        = Σ  notional
asset_exposure      = net notional per asset
leverage            = gross_exposure / equity
concentration       = |asset_exposure| / gross_exposure
unrealized_pnl      = Σ (mark − avg_entry) × qty × direction
realized_pnl        = cumulative from ledger, including funding and fees (K18)
drawdown            = decline from peak equity (equity_snapshots)
liq_distance        = |mark − liq_price| / mark    (liq_price from exchange, K6)
margin_utilization  = used margin / available margin
funding_cost        = periodic funding total
```

**Per strategy (K13):** the same metrics computed by `strategy_id`. In delta-neutral
setups, portfolio-level gross exposure alone is actively misleading;
`net_delta_per_asset` is reported inside the strategy. See `ARCHITECTURE.md` §9.

**Collateral (M5 — its own domain, not a field on the risk engine):**

```
margin_balance           per venue
maintenance_margin_rate  MMR — intra-venue leverage
margin_buffer            equity − maintenance margin
loan_to_value            LTV — extra-venue collateralized borrowing (V2)
```

**Scenario shock (M7.5):** cheap, because the engine is a pure function (L4).
`POST /risk/scenario` takes `{"BTC": -0.20, "ETH": -0.25}`, shocks the prices, re-runs
valuation and returns equity, margin buffer and distance to liquidation. For a leveraged
user this is the single most valuable feature in the product.

---

## 5. API

```
POST   /auth/login                          POST /auth/logout
POST   /auth/accept-invite

GET    /portfolio                     ?at=<RFC3339>    historical reconstruction
GET    /portfolio/history             ?from&to&interval
GET    /positions                     GET /positions/{id}
GET    /positions/{id}/lineage        events + prices that produced this number
PUT    /positions/{id}/strategy
GET    /pnl                           ?from&to
GET    /exposure                      GET /risk
POST   /risk/scenario                 price shock; models only, writes nothing (K56)
GET    /transactions                  ledger, cursor-paginated on seq
GET    /funding

GET    /integrations                  POST /integrations/binance
DELETE /integrations/{id}
GET    /data-quality                  open findings, worst first; ?history=true for closed
POST   /integrations/{id}/resync      rewind the walk; never writes a correction (K55)
GET    /assets                        canonical registry + alias resolution
GET    /transfers                     joined movements; unmatched legs are in /data-quality
POST   /transfers                     join two legs the matcher would not (K57)
DELETE /transfers/{id}                undo a join; touches no event (L2)
POST   /transfers/{out}/link/{in}     manual transfer matching
GET    /strategies                    POST /strategies
GET    /alerts
GET    /alert-rules                   PUT /alert-rules/{id}

GET    /stream/portfolio   (SSE)      GET /stream/risk   (SSE)
GET    /stream/positions   (SSE)
```

**Contract rules (all endpoints, no exceptions)** — see `ARCHITECTURE.md` §10:
all numbers are JSON strings; every response carries `as_of` and `freshness`; every total
in a response comes from one `valuation_run`; no endpoint writes ledger events; no
endpoint ever places an order.

---

## 6. Stack

| Layer | Choice | Note |
|---|---|---|
| Language | Go | |
| API | Huma v2 + chi | Handler-derived OpenAPI; no hand-written spec to drift |
| DB | PostgreSQL | `NUMERIC(38,18)`, RLS, JSONB, BRIN, advisory locks, `EXCLUDE USING gist` |
| DB access | pgx v5 + sqlc | decimal override; generated output is committed |
| Migration | goose | runs as `plimsoll_owner`, never the app role |
| Money | shopspring/decimal | K4 |
| Cache / bus | Redis | latest-price hash + SSE fan-out only (K28) |
| Realtime out | SSE | |
| Realtime in | Exchange WebSocket | `coder/websocket` — context-aware cancellation |
| Password | argon2id (`x/crypto/argon2`) | K16 |
| Session | opaque token, hashed in Postgres | not JWT — must be revocable (K16) |
| Encryption | AES-256-GCM, envelope | per-account DEK behind a `KeyProvider` interface (K25) |
| Rate limit | `x/time/rate`, two tiers | `WaitN(ctx, weight)` models endpoint cost (K24) |
| Logging | `log/slog` | structured, correlated to OTel traces |
| Observability | OpenTelemetry + Prometheus + Grafana | |
| Testing | stdlib + testify/require + go-cmp | integration tests need a real Postgres |
| TLS / routing | Caddy, same origin | no CORS, token unreachable from JS (K27) |
| Local & prod | Docker Compose | one file, environment overrides |
| Frontend | Next.js + TS + Tailwind + shadcn/ui + Lightweight Charts | |

**Versions are pinned at M0** in `go.mod`, the compose images and `package.json`, after
verifying what is current — not asserted from memory.

---

## 7. Repository Layout

See `../CLAUDE.md` §6 for the full tree with module-to-decision mapping.
Directories are created when the module is written, not in advance.

---

## 8. Milestones

| # | Deliverable | Exit criteria |
|---|---|---|
| **M0** ✅ | Skeleton + tenancy foundation | `compose up` → `/healthz`; goose migrate; sqlc generate; OTel trace visible; two DB roles; `tenancy.InTx` wrapper; accounts/sessions/invites; **tenant isolation test green with the application-level `WHERE` deliberately removed** |
| **M1** ✅ | Asset/instrument registry + ledger + position engine — **no network** | Fixture replay: spot average cost + realized PnL correct; time-scoped alias resolution tested; idempotency, order-independence and rebuild-equality tests green |
| **M2** 🟡 | Binance spot backfill | Real account history → ledger; idempotency holds across REST and WS paths; backfill resumes after interruption — **code complete, live verification pending** (see below) |
| **M3** ✅ | Portfolio + API + lineage | `GET /portfolio` correct; `GET /positions/{id}/lineage` opens a position down to its events |
| **M3.5** ✅ | Data quality + intra-venue transfers | Negative-balance / gap / unknown-symbol checks running; a spot ↔ futures transfer is not counted as a sale |
| **M4** ✅ | Market data + valuation | `price_ticks` populating; one `valuation_run` per response; USD price paths recorded; `freshness` populated; `GET /portfolio?at=` working. `GET /pnl` and the lineage price paths shipped with it; `GET /portfolio/history` deliberately deferred (K48) |
| **M5** ✅ | Perpetuals + collateral | USD-M perps in the registry (`PERPETUAL` + `TRADING` only), fills and funding normalized, the margin picture captured as one act and stored, `GET /risk` reporting equity, margin buffer, maintenance margin and per-position liquidation distance from one snapshot named in `as_of`, `GET /funding` summing payments per symbol from the ledger; a stale capture is served with `collateral_stale` and a missing one is `collateral_unavailable`, never a zero buffer; hedge mode refused (one-way only) |
| **M6** ✅ | Strategy + risk + alerting | Strategy tags that survive a projection rebuild (K30); a risk engine reporting gross AND directional leverage, so a delta-neutral basis trade does not read as 2× (K13); `GET /exposure` agreeing with `/portfolio` on one run; alert rules with hysteresis and cooldown — twenty crossings, one alert — delivered to Telegram or a webhook and recorded either way; SSE over LISTEN/NOTIFY (K51); the USD-M history walk M5 left uncalled, and the live futures stream as its trigger (F17, F19, F20); dashboard v1 |
| **M7** ✅ | Reconciliation | Classified findings (`missing_event` / `duplicate` / `rounding` / `unsupported`) decided from evidence rather than sign (K54); a register where a problem has a lifetime rather than a timestamp, so a disagreement lasting a day is one finding and not 288 (K53); `GET /data-quality`; a resync that rewinds the walk and writes no correction (K55); and `reconciliation_mismatch` — declared in M0 and never produced until now — reaching `/portfolio` from an open finding |
| **M7.5** ✅ | Scenario shock | `POST /risk/scenario` projects equity and margin buffer under a price shock. A shock names its asset and the unshocked hold still, so a hedged book is shown on both legs and no correlation is invented (K56); maintenance is recomputed from the venue's tier table at the shocked notional rather than scaled, because a shock worth modelling usually crosses a tier (F15); an uncaptured bracket table makes the buffer unavailable rather than larger |
| **M8** ✅ | Bybit + cross-venue transfers | Two sources into one ledger: a signed Bybit V5 client (B1), both history walks with the venue's own 30-day windows and cursor paging (B3), and normalizers over the two status enums Bybit publishes and Binance does not (B2). A withdrawal under one integration and a deposit under another are joined by txid (proof) or by amount and time (a guess, marked), one-to-one, with ambiguity refused; a leg with no other half becomes a data-quality finding naming what it is mistaken for; `GET/POST /transfers` and a manual join the user can undo (K57). **Not built, by decision:** Bybit trades, positions and funding — a Bybit portfolio is a later milestone |

**M8 is shipped, with its boundary drawn rather than drifted to.** Cross-venue matching is
proven against a real Postgres: two legs are joined, the ledger is byte-identical either side
of the match, a re-run produces the same links rather than a duplicate-key error, and an
unmatched leg becomes a finding that says what it is mistaken for. The Bybit adapter walks
both histories, resumes, pages by cursor, and refuses any status outside the published enum.

**B2 is the finding that shaped the milestone.** Bybit publishes the deposit and withdrawal
status enums; Binance publishes neither (F5, re-checked 2026-09-11 and still the garbled
fragment `0(0 Sent, 2 Approval 3 4 6)`). So `NormalizeBybitWithdrawal` exists and its Binance
counterpart still does not. An account moving coins from Binance to Bybit therefore produces a
deposit with no withdrawal to match -- a documentation gap, not a matcher failure, and K57
makes it a visible finding rather than silence.

**B5 made the key check better than Binance's.** `GET /v5/user/query-api` states `readOnly`
outright, where F8 had to infer it from booleans the page never defines. A key is accepted only
when the venue says read-only **and** every permission it holds is on a closed allowlist -- two
independent checks, because one is the venue's promise and the other is ours (K9, L13).

**What Bybit does not do here, deliberately:** trades, positions, funding. M8 connects the
venue for the half that feeds cross-venue matching. Half-building a second full ingest is how a
venue ends up with a normalizer nothing calls, which this project has done twice (K38, and
again in M5's futures walk) -- so the walks written here are wired into the worker in the same
commit that adds them.

**Still unverified, and therefore not assumed:** whether reading the two history endpoints
requires a granted permission at all, or whether `readOnly` alone suffices. The pages state
none. A key that needs one fails loudly at the first call with the venue's own error, which is
the right failure; inferring a permission model would be the wrong one.

**M2 is code complete and not shipped.** Every piece is written and tested against
recorded and documented fixtures: the signed REST client, the normalizer, the resumable
backfill, the live WebSocket stream, the single-writer lease and the supervisor. `make
test-integration` covers "backfill resumes after interruption" and "one identity for one
trade across REST and WS" against a fake exchange.

What is **not** proven, and will not be until a real read-only key exists:

| Exit criterion | Status |
|---|---|
| Real account history → ledger | **unproven** — no account has been connected |
| Idempotency across REST and WS, on real payloads | **partly** — proven on a derived fixture pair, not on one trade seen twice by a real account |
| Backfill resumes after interruption | proven, against a fake exchange |
| Rate limits respected; no 418 during a real backfill | **unproven** |

Contact with a real Binance account was deferred by decision on 2026-09-04. Two design
questions ride on that key and are answered defensively in the meantime rather than left
open: **F5** (what `myTrades?fromId=0` returns) is checked at runtime and raises
`backfill_incomplete` if the inference does not hold, and the **withdrawal status enum**
is undocumented, so `NormalizeWithdrawal` does not exist rather than guessing. Both are
recorded in `BINANCE-API-NOTES.md` §5.

**Shipped.** M0 and M1 are complete; the exit criteria above are covered by the test
suite (`make test`, `make test-integration`), and every invariant guard has been
mutation-tested — deliberately broken to confirm the test that guards it fails. M1 raised
four contradictions the documents could not all satisfy; they are resolved as K29–K32.

**M3 is shipped.** Both exit criteria are covered by integration tests against a real
Postgres: a ledger is appended, folded and read back as a portfolio whose numbers are the
fold's, and a position is opened down to every event that produced it with the state each
one left behind.

Two things were found while building it and are worth recording here rather than only in
the register:

- **Nothing was running the fold.** `projection.Project` shipped in M1, was tested, was
  rebuild-equal, and no process called it -- so on a live system the ledger would have
  filled while `positions` stayed empty. Every test in the package called `Project`
  directly, which is exactly why none of them could notice that production never did.
  Fixed in K38, and the test that guards it now runs a supervisor and never mentions the
  projector.
- **The lineage endpoint checks itself** (K43). It replays a position's events through the
  same engine the projector uses and compares the result against the stored row; a
  disagreement is reported as `lineage_mismatch` at severity `error` rather than served.

What M3 does **not** have, by decision rather than omission: no total. M4 owns prices, so
`GET /portfolio` reports subtotals per quote asset and carries `valuation_unavailable`
(K40). Adding realized PnL denominated in USDT to realized PnL denominated in BTC would
produce a number with no unit.

**M3.5 is shipped.** `asset_balances` folds beside the positions over the same events, so a
spot account can be asked what it holds rather than only what its exposures cost (K44), and
a transfer between two of its wallets is no longer a movement at all:

| M3.5 exit criterion | Status |
|---|---|
| Negative-balance check running | **done** — `negative_balance`, severity `error`, one reason per asset, and it goes quiet when the deposits pay for the fills |
| Unknown-symbol check running | **done** — a fee in an uncurated ticker raises `unknown_symbol` and the shortfall is exactly that fee (K45) |
| Gap check running | **done since M2** — `ws_gap` from the published worker state (K39) |
| A spot ↔ futures transfer is not counted as a sale | **done** — `TestASpotToFuturesTransferIsNotCountedAsASale`, through HTTP |

The transfer half was left until last because nothing produced a `TRANSFER` event: Binance
reports an intra-venue move through the wallet endpoint rather than through the spot stream,
and V1 ingests spot. Verifying that endpoint before planning changed the plan (F10): it
returns **one row per transfer** with the direction in its `type`, so there are no two halves
to match. K12's heuristic is what a venue that reports them separately needs, which is
cross-venue and stays in M8; `transfer_links` was not built here.

It also retired the convention the balance engine was holding. A transfer's quantity was to
be signed, positive in and negative out; with both endpoints named on the event the sign is
redundant and can only disagree with them, so it is now refused. A move between two wallets
of one integration folds to **no delta at all** — the account holds what it held. The signed
branch survives as the `external` endpoint, which is what M8 turns on, and which is also what
makes the internal case falsifiable: without an external transfer reaching the database, a
projector that skipped `TRANSFER` events entirely would produce numbers identical to one that
folds them correctly.

The milestone's success is mostly the absence of change, which needs a fourth assertion to be
worth anything: the position is unchanged, the realized PnL is unchanged, the balance is
unchanged, **and the transfer is visible in `GET /transactions`**. Without the last one, a
system that dropped the event on the floor would pass every other check.

**M0 comes first because tenancy cannot be retrofitted.** Adding `account_id` and RLS to
a schema that already holds a real ledger means rebuilding every table.

**M1 finishes before any network code.** Debugging the engine against live data is the
most expensive path available.

---

## 9. Test Strategy

Detail in `ARCHITECTURE.md` §12. The four invariant tests, each guarding a property the
whole system rests on:

1. **Idempotency** — the same event set applied twice leaves state unchanged
2. **Order independence** — canonically sorted events give identical state regardless of
   ingest order (K21)
3. **Rebuild equality** — `positions` equals a fold from zero, exactly (L3)
4. **Tenant isolation** — verified with the primary defence deliberately removed, so the
   RLS backstop is proven on its own (K15)

Beyond those: golden fixture replay, and reconciliation tested against a Binance testnet
account. Engines are pure functions and must be unit-testable without a database (L4).

---

## 10. Questions Resolved Before Coding

The original draft listed six open questions. All are now decided:

| Question | Decision | Ref |
|---|---|---|
| Multi-user or single-user? | **Multi-user in V1.** App-level scoping + RLS backstop; invite-provisioned accounts, opaque revocable sessions | K15, K16 |
| Base currency: USD or USDT? | **USD.** Stablecoins are priced, not assumed at 1.00; the price path is recorded and `assumed_peg` is flagged | K17 |
| Fees paid in another asset (BNB)? | **Stay in their own asset.** Never folded into `avg_entry_price`; a separate realized-PnL component converted at `event_time` | K18 |
| Is a spot "position" just a balance? | **Average cost is tracked.** Otherwise spot PnL cannot be produced | K5 |
| How far back does backfill go? | **The account's full lifetime**, staged and resumable. A fixed window poisons cost basis permanently | K26 |
| Multiple Binance sub-accounts? | `account → integration → sub-account` exists in the model from V1, even before it appears in the UI | K15 |

**Also settled:** average-cost accounting stays in V1, but the ledger must remain
**lot-derivable** — a V2 tax-lot projection (FIFO/LIFO/HIFO, per-venue cost basis) will
be recomputed from events, not bolted onto the schema. Storing average cost as the only
truth is therefore forbidden (K5).

---

## 11. Working Setup

The agent operating manual is `../CLAUDE.md`, duplicated byte-for-byte as `../AGENTS.md`.
It carries the invariants (L1–L15), the role split, and the definition of done. It is
read at the start of every session.

- **Roles:** Claude implements and writes the tests, test-first. Codex is an independent
  second-eye reviewer and does not write production code. Because the implementer also
  writes the tests, that review is the compensating control, not a formality.
- **One module per session.** Cross-cutting refactors get their own session.
- `make generate` (sqlc), `make migrate`, `make test` are part of the build loop.
- Exchange payloads are recorded into `testdata/fixtures/` first, with credentials
  stripped by a checked-in redaction script. Agents work against fixtures, not the live
  API.
- Binance endpoint details — symbol requirements, `listenKey` lifetime, `positionRisk`
  version, weight costs — are verified against the official documentation before being
  coded. Never from memory: a wrong constant here produces plausible, wrong numbers.
