# Binance API — verified notes

**Verified against the official documentation on 2026-09-03.** Every figure below was read
from `developers.binance.com`, not from memory and not from a blog post. `CLAUDE.md` §2
requires this because getting these details wrong produces plausible, wrong numbers.

Re-verify before M2 lands: Binance changes endpoint versions and weights without notice,
and two of the facts below already contradict what the ecosystem still documents widely.

| Source | URL |
|---|---|
| Spot REST | https://developers.binance.com/docs/binance-spot-api-docs/rest-api |
| Spot user data | https://developers.binance.com/docs/binance-spot-api-docs/user-data-stream |
| Spot WS API user data | https://developers.binance.com/docs/binance-spot-api-docs/websocket-api/user-data-stream-requests |
| Spot limits | https://developers.binance.com/docs/binance-spot-api-docs/rest-api/limits |
| USD-M user data | https://developers.binance.com/docs/derivatives/usds-margined-futures/user-data-streams |
| USD-M trades | https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/Account-Trade-List |
| USD-M income | https://developers.binance.com/docs/derivatives/usds-margined-futures/account/rest-api/Get-Income-History |
| USD-M position risk | https://developers.binance.com/docs/derivatives/usds-margined-futures/trade/rest-api/Position-Information-V3 |
| Wallet deposits | https://developers.binance.com/docs/wallet/capital/deposite-history |
| API key permission | https://developers.binance.com/docs/wallet/account/api-key-permission |
| Spot market streams | https://developers.binance.com/docs/binance-spot-api-docs/web-socket-streams |
| Spot market data REST | https://developers.binance.com/docs/binance-spot-api-docs/rest-api/market-data-endpoints |

---

## 1. The three findings that change M2

### F1 — Spot `listenKey` is gone

Following the 2025-04-07 announcement, listenKey documentation for
`wss://stream.binance.com` was **removed**. Spot user data now arrives by subscribing on
the **WebSocket API**:

- `userDataStream.subscribe` — requires an authenticated connection established with
  `session.logon`, and **that requires Ed25519 keys**
- `userDataStream.subscribe.signature` — the per-request signature variant, usable with a
  normal HMAC key
- 2 IP weight per subscribe request
- `session.subscriptions` lists what is active; `userDataStream.unsubscribe` ends one

**Consequence for K25:** the credential model has to hold an Ed25519 private key, not only
an HMAC secret, if we want the authenticated path. The signature variant avoids that and
is the smaller change. This is a decision M2's plan has to take explicitly.

> **Filled in 2026-09-04 while writing M2 task 7**, from `web-socket-api.md` and
> `user-data-stream.md`. The row that said the spot subscription's expiry was "not
> documented on the page read" is answered, and it is the *connection* that expires:
>
> - Base endpoint: `wss://ws-api.binance.com:443/ws-api/v3`
> - **"A single connection to the API is only valid for 24 hours; expect to be disconnected
>   after the 24-hour mark."** So a reconnect is routine, not exceptional, and a stream that
>   does not reconnect stops working every day by design. Every reconnect is a gap.
> - "The WebSocket server will send a `ping frame` every 20 seconds. If the WebSocket server
>   does not receive a `pong frame` back from the connection within a minute the connection
>   will be disconnected."
> - Subscribe request, verbatim shape: `{"id": "...", "method":
>   "userDataStream.subscribe.signature", "params": {"apiKey": ..., "timestamp": ...,
>   "signature": ...}}`, with optional `recvWindow` (max 60000). Weight 2. Response:
>   `{"id": "...", "status": 200, "result": {"subscriptionId": 0}}`.
> - **Events arrive wrapped**: `{"subscriptionId": N, "event": {...}}`, "sent as JSON in
>   text frames, one event per frame". The normalizer takes what is under `event`.
> - `eventStreamTerminated` is a documented event, sent on unsubscribe, logout, or listen
>   token expiry.
> - A session supports up to 1,000 active subscriptions, and 65,535 over its lifetime.
> - **Signing is not the REST rule.** REST signs the query string exactly as sent,
>   percent-encoding included; the WebSocket API signs every param except `signature`,
>   sorted alphabetically by name, joined as `name=value` with `&`, raw. Signing one the
>   other's way mints a valid signature for a request nobody made.

**USD-M futures still uses listenKey**, so the two markets do not share a realtime path:

| | Spot | USD-M |
|---|---|---|
| Mechanism | WS API `userDataStream.subscribe` | `listenKey` |
| Start | — | `POST /fapi/v1/listenKey` (weight 1) |
| Keepalive | — | `PUT /fapi/v1/listenKey` (weight 1) |
| Close | `userDataStream.unsubscribe` | `DELETE /fapi/v1/listenKey` (weight 1) |
| Expiry | not documented on the page read | 60 minutes without a keepalive |

The docs suggest pinging "about every 60 minutes", which is the expiry itself — ping well
inside it. Every listenKey call needs the `X-MBX-APIKEY` header.

### F2 — Futures history stops at three months

Both `/fapi/v1/userTrades` and `/fapi/v1/income` state it plainly:

> "Only support querying trade in the past 3 months."
> "Income history only contains data for the last three months."

**The ledger therefore cannot be complete for USD-M before that date, ever, from these
endpoints.** This is not a bug to fix; it is a fact to report. L11 says degraded and
visible beats confident and wrong, and the M0 reason-code set has nothing for it —
`backfill_incomplete` says "not finished yet", which is a different and recoverable claim.
M2 needs a distinct reason, e.g. `history_truncated`, meaning "the venue will not give us
this and never will".

### F3 — `tranId` is unique per `incomeType`, not globally

> "tranId is unique in the same incomeType for a user."

So an income event's identity **must** carry the type:
`usdm:income:FUNDING_FEE:98765432`. This is exactly the shape `ARCHITECTURE.md` §3 already
documents — now confirmed from the source rather than assumed. Dropping `incomeType` from
the key would collide a funding fee with a commission and silently deduplicate one away
(L5, K19).

### F4 — There is no "all my spot trades" endpoint

`GET /api/v3/myTrades` requires `symbol`. So does `allOrders`. The only account endpoint
that accepts an omitted symbol is `openOrders` (weight 6 with a symbol, 80 without), and
open orders are not history.

`GET /sapi/v1/accountSnapshot` does not rescue this either: **IP weight 2400**, and it
"supports query within the last one month only". It is not a discovery tool, it is a
liability.

**So symbol discovery has to be solved by us, and it decides how M2's backfill is shaped.**
An asset that was bought and fully sold leaves no trace in current balances and none in
deposit or withdrawal history — the only record is its trades, which cannot be found
without already knowing the symbol. Any discovery built from balances and transfers has
that hole in it, and K26 exists precisely to refuse holes.

The approach that has none: probe **every** spot symbol from `exchangeInfo` once, with
`myTrades?symbol=X&limit=1`. Roughly 2–3k symbols at weight 20 is 40–60k weight — under
ten minutes of a dedicated budget, once, for a full-history import. Symbols that come back
empty are never queried again.

### F5 — The 24-hour window binds `startTime`/`endTime`, not `fromId`

The constraint is written as "the time between `startTime` and `endTime` can't be longer
than 24 hours" — it is a constraint on the pair. The `fromId` path carries no documented
time restriction and returns trades with id ≥ the value given.

So a spot symbol's whole history is walked with `fromId` and `limit=1000`, one weight-20
request per thousand trades, with no time chunking at all. The 24-hour window then matters
only for the gap-resync path, where the window is known and small.

**Confidence:** this is read from the phrasing, not from a sentence that states it
outright. It is the first thing M2 verifies when it records its fixtures.

> **Corrected 2026-09-04.** The sentence that followed said the backfill "falls back to
> 24-hour chunks if it turns out to be wrong". That was over-pessimistic and it is not what
> task 6 built: a pure 24-hour walk from spot's 2017 launch is roughly 3,300 windows per
> symbol at weight 20, which is not a backfill anyone runs. The walk keeps `fromId` and
> checks the inference at runtime instead -- see section 5.

---

## 2. Endpoints M2 needs

| Endpoint | Weight | Limit | Time window | History depth |
|---|---|---|---|---|
| `GET /api/v3/myTrades` | **20** (5 with `orderId`) | max 1000, default 500 | **≤ 24 hours** | not stated |
| `GET /api/v3/account` | 20 | — | — | — |
| `GET /fapi/v1/userTrades` | 5 | max 1000, default 500 | **≤ 7 days** | **3 months** |
| `GET /fapi/v1/income` | **30** | max 1000, default 100 | default 7 days | **3 months** |
| `GET /fapi/v3/positionRisk` | 1 | — | — | — |
| `GET /sapi/v1/capital/deposit/hisrec` | 1 | max 1000, default 1000 | **≤ 90 days** | — |
| `GET /sapi/v1/capital/withdraw/history` | 1 | max 1000 | **≤ 90 days** | — |
| `GET /api/v3/exchangeInfo` | see docs | — | — | — |
| `GET /sapi/v1/accountSnapshot` | **2400** | 7–30 snapshots | — | **1 month** |
| `POST/PUT/DELETE /fapi/v1/listenKey` | 1 | — | — | — |

Pagination differs by endpoint and the differences matter:

- **`myTrades`** — `fromId` returns trades with id ≥ that value. Without it, the most
  recent are returned.
- **`userTrades`** — `fromId` **cannot be sent together with** `startTime`/`endTime`. So a
  chunked historical walk and an id-based walk are two different strategies, not one with
  an optional parameter.
- **`income`** — `page`/`limit`, not `fromId`. Offset pagination over a moving window is
  the one that can skip or repeat rows if the window shifts underneath it; the chunk
  boundaries have to be pinned by time, not by page.
- **deposits/withdrawals** — `offset`/`limit`.

`positionRisk` is **V3** (`/fapi/v3/positionRisk`), not the V2 most examples still show.
K6 reads the exchange's liquidation price from here rather than computing it, so the
version and its field names are load-bearing.

## 3. Rate limits

Exact header names, quoted from the docs:

```
X-MBX-USED-WEIGHT-(intervalNum)(intervalLetter)
X-MBX-ORDER-COUNT-(intervalNum)(intervalLetter)
Retry-After
```

Interval letters are `S` / `M` / `H` / `D`. So the header to read on a normal request is
`X-MBX-USED-WEIGHT-1M`.

- **429** — a rate limit was broken; `Retry-After` says how long to wait.
- **418** — the IP was auto-banned for continuing to send after 429s. Bans scale from
  **2 minutes to 3 days** for repeat offenders.

> "Limits are based on the IPs, not the API keys."

**This confirms K24.** Two integrations belonging to two different accounts, running from
one server, share one budget. A per-key limiter alone would let one account's backfill get
another account's IP banned for three days — which is why K24 is two-tier: per-integration
weight accounting plus a shared per-IP gate.

The per-minute weight ceiling is **not hardcoded**: `/api/v3/exchangeInfo` returns a
`rateLimits` array with `RAW_REQUESTS`, `REQUEST_WEIGHT` and `ORDERS`. Read it at connect
time. A hardcoded number is a number that will be wrong after Binance changes it and we
will not notice until the ban.

## 4. Read-only key verification (K9, L13)

> **Corrected 2026-09-03** while writing M2 task 2. The earlier version of this section
> listed six fields and asserted that `enableFutures` is a read permission. Both were
> wrong: the documented response carries **thirteen** fields, and the page states no
> semantics for any of them. The paragraph below replaces it.

`GET /sapi/v1/account/apiRestrictions` — weight 1 (IP), requires the `X-MBX-APIKEY` header
and a `timestamp`; `recvWindow` is optional and capped at 60000. The response example given
on the page, verbatim and complete:

```json
{
  "ipRestrict": false,
  "createTime": 1623840271000,
  "enableReading": true,
  "enableWithdrawals": false,
  "enableInternalTransfer": true,
  "enableMargin": false,
  "enableFutures": false,
  "permitsUniversalTransfer": true,
  "enableVanillaOptions": false,
  "enableFixApiTrade": false,
  "enableFixReadOnly": true,
  "enableSpotAndMarginTrading": false,
  "enablePortfolioMarginTrading": true
}
```

**The page documents no meaning for any field.** Neither does the account-management FAQ,
which says only that permissions beyond reading should not be enabled without an IP
restriction, and that withdrawals require one. So the semantics of `enableFutures`,
`enableInternalTransfer` and `permitsUniversalTransfer` are **not** verified, and the
previous claim that `enableFutures` is a read permission had no source behind it.

That unresolved question decides the rule rather than blocking it. K9 says an
over-permissioned key is rejected, and under uncertainty the safe reading of "permission"
is the broad one. So `integration.Verify` **allows only a known list of read permissions to
be true** — `enableReading` and `enableFixReadOnly` — and rejects every other boolean that
is true, named or not. A field Binance adds next year is rejected by default, which is the
only version of this check that cannot silently start accepting a trading key.

Three fields are explicitly **not** capabilities and never cause a rejection: `createTime`
(a timestamp), `tradingAuthorityExpirationTime` (a timestamp; absent from the example but
documented elsewhere), and `ipRestrict` — which is a *restriction*, so `true` is the safer
key and rejecting it would be exactly backwards.

The cost of the broad reading is that a user whose key also has futures enabled is asked to
issue a separate key for us. That is the correct trade in M2, which is spot-only. **M5 must
resolve `enableFutures` against a real response before it can read futures data**, and if it
turns out to permit order placement then it stays rejected and futures reading needs a
different answer.

## 5. What is still unverified

> **Decided 2026-09-04.** Contact with a real Binance account is deferred as far as the
> project allows, so nothing below will be settled by recording a payload in the near term.
> Where an item shapes code, the code takes the defensive branch and says so; it does not
> wait. `plimsollctl record` can settle any of these in one command if a key ever appears.

**F5 is the one that shapes M2.** What the documentation *does* state is that `fromId`
fetches from a trade id and that the 24-hour limit binds `startTime`/`endTime`. What it does
not state is what `fromId=0` with no time range returns. The plan inferred "the oldest
trades"; the inference is not verified.

Abandoning `fromId` is not the safe alternative — a pure 24-hour walk from spot's 2017 launch
is roughly 3,300 windows per symbol at weight 20, which is not a backfill anyone runs. So
task 6 keeps `fromId` and **checks the inference at runtime instead of assuming it**: if the
first page returned for `fromId=0` is not contiguous with the pages that follow, the walk
stops and raises `backfill_incomplete` in `freshness` (L11) rather than reporting a history
it has silently truncated. Degraded and visible beats confident and wrong, and this costs
nothing to build.


**F4 has a residual hole that discovery cannot close.** The sweep probes every symbol
`exchangeInfo` names, which is every symbol *currently listed*. A pair delisted outright
before the sweep runs is no longer named, so it cannot be probed, and an asset acquired and
fully sold on it leaves no trace anywhere else -- not in balances, not in deposits, not in
withdrawals. Discovery is therefore complete with respect to the list Binance will give us,
not with respect to the account's history. Recorded here rather than papered over: it
belongs in `freshness` (L11), and the honest fix needs a symbol list Binance does not
publish.

**Two more gaps found while wiring the worker (2026-09-04), both recorded rather than
papered over:**

- **`myTrades` does not accept `fromId` together with a time range.** rest-api.md
  enumerates the supported combinations: `symbol`; `symbol + orderId`; `symbol + startTime`;
  `symbol + endTime`; `symbol + fromId`; `symbol + startTime + endTime`;
  `symbol + orderId + fromId`. So the historical walk and the gap replay are two strategies,
  not one with an optional parameter, and a full windowed page is halved rather than paged
  (K35). The client now refuses the combination before sending it, because a rejected
  request in the replay path reads as an empty window.
- **A deposit made while the stream is healthy is not picked up until the next walk.**
  `balanceUpdate` is not normalized -- it reports a delta with no id to deduplicate on --
  and the deposit walk stops once its scope completes. A deposit during a *stream gap* is
  caught, because the replay reads the window. One during normal operation waits for the
  next worker start. Not a design decision, a gap: closing it needs either a periodic
  re-walk or an identity for `balanceUpdate`.

Recorded so the M2 plan does not quietly assume them:

- The exact spot `REQUEST_WEIGHT` ceiling per minute — read from `exchangeInfo` instead.
- Whether spot trade history has a depth limit comparable to the futures three months.
- Whether `enableFutures` grants futures **trading** or only futures **reading**. Neither
  the API-key-permission page nor the account FAQ says. M2 rejects it either way; M5
  cannot proceed on futures without an answer. See §4.
- Whether `enableInternalTransfer` and `permitsUniversalTransfer` can move funds off the
  account or only between the user's own wallets. Rejected either way for the same reason.
- **Withdrawal history cannot be normalized yet — two undocumented facts, both load-bearing.**
  Checked twice on 2026-09-04, on the withdraw-history and withdraw pages.
  1. The **status enum is not published**. The only text is the garbled fragment
     `0(0 Sent, 2 Approval 3 4 6)` under the query parameter. Which code means "completed"
     decides whether coins are recorded as having left the account, and getting it wrong in
     either direction is a wrong balance.
  2. The **timezone of `applyTime` / `completeTime` is not stated**. They arrive as
     `"2019-10-12 11:12:02"`, not as epoch milliseconds like every other endpoint. An eight
     hour error would corrupt the canonical order (L7) and every time-windowed
     reconciliation.

  Deposits have neither problem: their status list is published in full and `insertTime` is
  epoch milliseconds, so `NormalizeDeposit` exists and `NormalizeWithdrawal` does not.
  Encoding a remembered enum into append-only financial rows is exactly what `CLAUDE.md` §2
  forbids. If this is ever settled, note that `raw` is stored verbatim (L15), so the fix is
  a replay rather than a migration.
- The complete `incomeType` enum. Eight were listed on the page read
  (`TRANSFER`, `WELCOME_BONUS`, `REALIZED_PNL`, `FUNDING_FEE`, `COMMISSION`,
  `INSURANCE_CLEAR`, `REFERRAL_KICKBACK`, `COMMISSION_REBATE`) and "14 additional types"
  were not enumerated. The normalizer must reject an unknown type loudly rather than map
  it to something plausible.
- USD-M user data event payloads (`ACCOUNT_UPDATE`, `ORDER_TRADE_UPDATE`) and the futures
  websocket base URL.
- Whether the spot WS API subscription expires, and what keeps it alive.
- SBE versus JSON. SBE is offered; JSON is the one whose payload we can store verbatim in
  `raw` and read six months later (L15), which is an argument on its own.

---

## 6. Market data (M4)

**Verified against the official documentation on 2026-09-09**, from the same repository
that backs `developers.binance.com`:
`raw.githubusercontent.com/binance/binance-spot-api-docs/master/{web-socket-streams,rest-api}.md`.

### F6 — Market data needs no key, and there is an endpoint that cannot carry account data

This is the finding the M4 plan rests on, so it was checked two ways rather than one.

**By rule.** `rest-api.md` §Request Security: *"If unspecified, the security type is
`NONE`."* and the table reads *"`NONE` — Public market data"*. Neither
`### Symbol price ticker` nor `### Kline/Candlestick data` carries a security type in its
heading, so both are `NONE`: no `X-MBX-APIKEY`, no signature.

**By construction.** `web-socket-streams.md` §General WSS information:

> The base endpoint **wss://data-stream.binance.vision** can be subscribed to receive
> **only** market data messages. User data stream is **NOT** available from this URL.

That is stronger than a convention. Connecting the price feed there makes "this connection
cannot carry account data" a property of the transport rather than a promise the code
keeps, which is the same reasoning that puts the ledger's append-only rule in a `GRANT`
rather than in a code review (L2).

**Consequence for the milestone order:** M4 is not blocked behind M2. M2 cannot close
without a real read-only key, because its exit criterion is real account history. Prices
are the same for every account, so M4 can be built and verified today.

### F7 — `!miniTicker@arr` omits what did not change, so the stream is never a snapshot

`web-socket-streams.md` §All Market Mini Tickers Stream:

> 24hr rolling window mini-ticker statistics for all symbols that changed in an array.
> **Note that only tickers that have changed will be present in the array.**

A symbol absent from a push has **not** lost its price — it has not traded in that second.
Treating absence as "no price" would blank the valuation of every thinly traded asset the
account holds, one second at a time, and the total would flicker for a reason no lineage
could explain.

Two design consequences, both in the M4 plan:

- A **REST snapshot on start** is mandatory, not an optimisation. Without it the feed has
  no price for anything until that symbol happens to trade.
- The recorder holds a last-known price per instrument and only writes a `price_ticks` row
  when a push actually names the symbol. The *age* of that price is then real, and
  `price_stale` (L11) is computed from it rather than from when we last looked.

| | |
|---|---|
| Stream name | `!miniTicker@arr` |
| Update speed | `1000ms` |
| Event type | `24hrMiniTicker` |
| Last price field | `c` ("Close price"), a **string** — parsed with `decimal`, never a float (L1) |

**The `"e"` / `"E"` trap applies here too.** The payload carries both `"e"` (event type,
string) and `"E"` (event time, number), and `encoding/json` falls back to case-insensitive
matching when no exact tag matches. This bit the normalizer once and the stream ingester
once; the market-data decoder reads by exact key from `map[string]json.RawMessage` for the
same reason.

### F8 — The market stream's lifetime and keepalive, quoted

`web-socket-streams.md` §General WSS information and §WebSocket Limits:

> A single connection to **stream.binance.com** is only valid for 24 hours; expect to be
> disconnected at the 24 hour mark.

> The WebSocket server will send a `ping frame` every 20 seconds. If the WebSocket server
> does not receive a `pong frame` back from the connection within a minute the connection
> will be disconnected. When you receive a ping, you must send a pong with a copy of ping's
> payload as soon as possible.

> A single connection can listen to a maximum of 1024 streams.

> There is a limit of **300 connections per attempt every 5 minutes per IP**.

> WebSocket connections have a limit of 5 incoming messages per second.

The 24-hour ceiling matches spot user data (F1), and the two are still verified separately:
they are different services and a shared number today is a coincidence, not a contract.

There is also an explicit warning shot, which the user-data stream has no equivalent of:

> `serverShutdown` event is sent when the server is about to shut down.

That is worth handling rather than waiting for the socket to drop — it turns a gap the
recorder has to detect into one it is told about in advance.

**One connection is enough.** `!miniTicker@arr` is a single stream, so the 1024 ceiling is
irrelevant and the 300-connections-per-5-minutes limit only binds a reconnect storm.
Prices are not per account: the feed belongs to the process, never to an integration, or an
IP-wide budget would be multiplied by the number of users (K24).

### F9 — The two REST endpoints, with their weights

| Endpoint | Purpose | Weight | Notes |
|---|---|---|---|
| `GET /api/v3/ticker/price` | Snapshot of every symbol in one call | **4** with `symbol` omitted | *"If neither parameter is sent, prices for all symbols will be returned in an array."* Weight 2 for a single symbol, so asking for all of them is **cheaper than asking for three**. |
| `GET /api/v3/klines` | Historical price at a past instant | **2** | `interval=1m` is supported; `limit` default 500, **maximum 1000**; *"`startTime` and `endTime` are always interpreted in UTC"* |

`/api/v3/ticker/price` at weight 4 for the whole market is what makes the start-up snapshot
and the post-gap refill affordable against the shared per-IP budget the account workers are
already spending (K24).

`klines` at `1m` is exactly the resolution K7 chose for `price_ticks`, which means a
historical backfill and the live recorder write the same shape of row and `?at=` cannot
tell which one filled a given minute. 1000 rows per call is a little under 17 hours of
minutes, so a year of one symbol is roughly 530 calls at weight 2.

### Still unverified for M4

- **How far back `klines` actually serves.** The documentation states no floor. It is not
  worth guessing: the historical backfill asks for the oldest window it wants and records
  what it gets, raising `history_truncated` (the permanent one, not `backfill_incomplete`)
  if the venue answers from a later date than requested.
- **Whether `wss://data-stream.binance.vision` carries the same 24-hour ceiling and ping
  contract as `stream.binance.com`.** The quoted lifetime names `stream.binance.com`
  specifically. The recorder therefore treats the ceiling as applying and reconnects on its
  own schedule regardless — being early costs one reconnect, being late costs a gap.
- Reference price streams (`<symbol>@referencePrice`) exist and are per-symbol only, with a
  `null` documented when no reference price is available. Not used: there is no all-market
  form, and one subscription per symbol is the design F7's snapshot exists to avoid.

---

## 7. Intra-venue transfers (M3.5)

Read on **2026-09-09** against `developers.binance.com`. Everything below is quoted from a
page that was actually opened; where a value could not be found on the page, that is stated
rather than filled in from a plausible memory.

### F10 — One row is one transfer, and the direction is in its `type`

`GET /sapi/v1/asset/transfer` — *Query User Universal Transfer History*, weight **1** (IP).

`type` is a **required** parameter, and it is a direction rather than a category: the 32
documented values are ordered pairs — `MAIN_UMFUTURE` and `UMFUTURE_MAIN` are two different
queries. So "every transfer this account made" is not one call; it is one call per direction
you care about.

The response is a page of rows:

```json
{ "total": 2, "rows": [
  { "asset": "USDT", "amount": "1", "type": "MAIN_UMFUTURE",
    "status": "CONFIRMED", "tranId": 11415955596, "timestamp": 1544433328000 } ] }
```

**One row per transfer, carrying both endpoints.** This is the finding that reshapes M3.5.
K12's matching heuristic — same asset and amount within fee tolerance, inside a time window,
`txid` when available — exists because a venue can report the two halves of a movement
separately and leave the joining to the reader. Here it does not: the row *is* the movement,
and both wallets are named by `type`. Intra-venue transfer matching is therefore not a
heuristic problem at all, and building a manual-resolution queue for it would be building a
UI for a problem this endpoint does not have. The heuristic is still needed cross-venue,
which is M8.

Paging is `current` (1-based) and `size` (**max 100**, default 10) with `total` returned —
offset paging, not keyset. Offset paging over a table that receives new rows shifts pages
under the reader, so the walk is windowed by `startTime`/`endTime` rather than trusted to
stay still.

Quoted, on range: *"Support query within the last 6 months only. If startTime and endTime not
sent, return records of the last 7 days by default"*. Six months, not three (futures income)
and not unbounded (spot trades) — a third horizon, and the backfill's history-truncated
boundary for this scope.

### F11 — The status enum is not published

The page shows `status` as a string and gives exactly one value, `"CONFIRMED"`, in the
response example. It **does not enumerate** the possible values anywhere. A web search
returns "CONFIRMED / FAILED / PENDING" — from search-result text, not from the page, which is
not a source this project encodes into append-only financial rows (`CLAUDE.md` §2).

The consequence is a whitelist rather than a blacklist: `CONFIRMED` is recorded and every
other value is refused loudly. The failure mode of guessing wrong here is smaller than for
withdrawals (§5) — an intra-venue transfer nets to zero on the account's balance either way —
but "smaller" is not "absent": a `PENDING` row recorded as complete misstates which wallet
holds the money, which is exactly the question M5 asks.

### F12 — The same movement is reported twice, by two endpoints

A spot → USD-M transfer appears **both** as a `MAIN_UMFUTURE` row in universal-transfer
history **and** as an `incomeType: TRANSFER` row in `GET /fapi/v1/income` (weight 30,
*"Income history only contains data for the last three months"*).

This is a double-count trap wearing the costume of thoroughness. M5 ingests futures income
for funding and realized PnL, and folding its `TRANSFER` rows as balance changes as well
would move the money twice. Recorded here, before the code that would do it exists: the
futures income normalizer must skip `TRANSFER` and say why, because the wallet endpoint has
already reported it.

### F13 — Identity includes the type, not only the `tranId`

`tranId` is an int64 per row. F3 already found that on futures income it is unique per
`incomeType` rather than globally; nothing on this page claims a stronger guarantee for
transfers. Identity is therefore `transfer:<type>:<tranId>` — the same shape as every other
venue event id, and one that cannot collide across two directions even if the venue reuses a
number between them (L5).

### Still unverified for M3.5

- The status values other than `CONFIRMED`. Whitelisted rather than guessed (F11).
- Whether a transfer can carry a fee. No fee field appears in the row, and none is documented;
  if one exists it would arrive in `raw` (L15) and the fold would need L9 applied to it.
- Whether `tranId` is globally unique across transfer types. Assumed not, which is the safe
  direction: a wider identity cannot merge two transfers, a narrower one can.

---

## 8. USD-M perpetuals and collateral (M5)

Read on **2026-09-10** against `developers.binance.com`. Everything below is quoted from a
page that was actually opened; where a value could not be read off the page, that is stated
rather than filled in from a plausible memory.

### F14 — `positionRisk` does not carry maintenance margin, and the account endpoint does

`GET /fapi/v3/positionRisk` — *Position Information V3*, weight **1** (IP). Its fields:

```
symbol · positionAmt · entryPrice · markPrice · unRealizedProfit · unRealizedProfitRate
roiRate · leverage · maxNotionalValue · liquidationPrice · marginType · isAutoAddMargin
positionSide · notional · isolatedCreated · adlQuantile · marginRatio · updateTime
```

`liquidationPrice` is here, which is what K6 reads rather than computes. **`maintMargin` is
not.** Maintenance margin lives on `GET /fapi/v3/account` — *Account Information V3*, weight
**5** — as `totalMaintMargin` for the account and `positions[].maintMargin` per position,
beside `totalMarginBalance`, `totalWalletBalance`, `totalUnrealizedProfit` and
`availableBalance`.

So the margin buffer is `totalMarginBalance - totalMaintMargin`, and both halves come from
one response. The consequence for the design is that **collateral is a snapshot of the
account endpoint while liquidation price is a snapshot of positionRisk**, and the two must be
captured as one act at one instant. A buffer from 12:00:00 next to a liquidation price from
12:00:30 is a screen describing two different accounts — the exact failure L10 exists to
prevent, arriving through a door L10 did not name because it is not about prices.

### F15 — the MMR table is its own endpoint, and M7.5 does not exist without it

`GET /fapi/v1/leverageBracket` — *Notional and Leverage Brackets*, weight **1**:

```json
{ "symbol": "...", "notionalCoef": ..., "brackets": [
  { "bracket": 1, "initialLeverage": 75, "notionalCap": 10000,
    "notionalFloor": 0, "maintMarginRatio": 0.0065, "cum": 0.0 } ] }
```

`account.maintMargin` is today's number at today's notional. A price shock changes the
notional, and a large enough one crosses a bracket into a **higher** `maintMarginRatio` — so
the scenario shock's whole point, "how far am I from liquidation if BTC drops 20%", cannot be
answered by scaling the current maintenance margin. It needs the table.

Which is why the brackets are captured in M5 rather than in M7.5: M7.5 is a pure function
over data (L4), and a pure function cannot go and fetch what it was not given. Recording this
now is the difference between M7.5 being a week and M7.5 discovering it needs an ingest path.

### F16 — the income enum is not fully published, and paging is by page number

`GET /fapi/v1/income` — weight **30**. Quoted: *"Income history only contains data for the
last three months."* · *"If `incomeType` is not sent, all kinds of flow will be returned"* ·
*"If `startTime` and `endTime` are not sent, the recent 7-day data will be returned."*

Parameters are `symbol`, `incomeType`, `startTime`, `endTime`, `page`, `limit` (max 1000,
default 100). Page-number paging over a moving table again (F10), so the walk is windowed for
the same reason.

The rendered page lists `TRANSFER`, `WELCOME_BONUS`, `REALIZED_PNL`, `FUNDING_FEE`,
`COMMISSION`, `INSURANCE_CLEAR`, `REFERRAL_KICKBACK`, `COMMISSION_REBATE` and then **refers
to fifteen further types it does not display**. So the enum cannot be read off the page in
full, and the treatment is F11's: map the types V1 folds, and refuse an unrecognized one
loudly rather than let an unknown cash flow through as zero.

Also quoted, confirming F3 in the venue's own words (typo included): *"`trandId` is unique in
the same `incomeType` for a user."* The identity is `usdm:income:<incomeType>:<tranId>`, and
F12 still stands: `TRANSFER` rows here are the wallet endpoint's transfers seen a second time
and are skipped, or the money moves twice.

### F17 — the futures user stream moved, and the old URL has been dead for five months

Quoted from *Important WebSocket Change Notice*: *"Legacy URLs will remain available until
**2026-04-23**, after which they will be permanently decommissioned."* · *"After the upgrade,
any connections not migrated will ONLY be able to receive data from
`wss://fstream.binance.com/public`."*

| Legacy (dead since 2026-04-23) | Now |
|---|---|
| `wss://fstream.binance.com/ws` | `wss://fstream.binance.com/public` — high-frequency data |
| `wss://fstream.binance.com/stream` | `wss://fstream.binance.com/market` — regular market data |
| | `wss://fstream.binance.com/private` — **user data** |

The listenKey goes in the query string: `?listenKey=<key>&events=ORDER_TRADE_UPDATE`.
`POST /fapi/v1/listenKey` mints one (weight 1); *"The stream will close after 60 minutes
unless a keepalive is sent"*, refreshed with `PUT /fapi/v1/listenKey`.

This is F1 wearing different clothes, and it is the reason that rule exists: a futures stream
written from memory in 2026 connects to a URL that stopped serving user data in April, and
the symptom is a worker that connects successfully and never receives an event — which looks
exactly like an account with no activity.

The existing code is not affected. Spot uses `wss://ws-api.binance.com` (F1) and market data
uses `wss://data-stream.binance.vision` (F6); neither is `fstream`.

### F18 — futures fills are per symbol, so discovery happens again

`GET /fapi/v1/userTrades` — *Account Trade List*, weight **5**, `symbol` **required**.
Fields: `buyer, commission, commissionAsset, id, maker, orderId, price, qty, quoteQty,
baseQty, marginAsset, realizedPnl, side, positionSide, symbol, pair, time`.

F4's problem again: there is no "all my futures trades". The spot discovery sweep (K33) is
the shape the futures walk needs too, over the futures symbol universe rather than the spot
one. `positionSide` is `BOTH` in one-way mode, which is V1's only mode.

`realizedPnl` arrives on the fill. The position engine computes realized PnL itself from the
average-cost fold (K5), so the venue's number is a **reconciliation input, not an ingest
input** — storing it as truth would create the second source of truth L3 forbids, and folding
it as well as computing it would double the number.

### F19 — the two USD-M history endpoints answer for three months, in seven-day windows

`GET /fapi/v1/userTrades` (weight 5, signed): *"The time between `startTime` and `endTime`
cannot be longer than 7 days"* · *"Only support querying trade in the past 3 months"* ·
*"`fromId` cannot be sent with `startTime` or `endTime`"* · limit max 1000, default 500.

`GET /fapi/v1/income` (weight **30**, signed): *"Income history only contains data for the last
three months"* · default limit 100, max 1000, with a `page` parameter.

Two consequences.

**The futures fill walk is time-windowed, unlike spot's.** Spot pages by trade id because its
history reaches back to 2017 and a time walk would be thousands of requests per symbol — which
forced the F5 inference about what `fromId=0` returns, checked at runtime because the page does
not say. Here the venue answers for three months at most, so thirteen windows cover the whole of
what exists and nothing has to be inferred at all.

**Income is walked before fills, because it is the discovery.** It takes no symbol and returns
every cash flow at once, so one walk names every perpetual the account has touched. Sweeping the
listed contracts instead would be several hundred symbols walked to find the four that matter —
at weight 5 each, for nothing.

The three-month horizon is `history_truncated`, not `backfill_incomplete`: it is a permanent
claim about what can be known, and telling a user to wait for something that will not arrive is
the confident-and-wrong failure L11 exists to reject.

### F20 — Go's JSON decoder matches keys case-insensitively, and this venue uses `e` and `E`

Found while writing the futures stream trigger, by a test that expected `"BTCUSDT"` and got
`"BUY"`.

`encoding/json` falls back to a **case-insensitive** match when no exact tag matches — and on
this venue's WebSocket payloads the case is the field. `ORDER_TRADE_UPDATE` carries `e` (event
type) beside `E` (event time), and its order object carries `s` (symbol) beside `S` (side). A
struct tagged `json:"s"` is filled by whichever the decoder reaches: in practice the side. The
symbol comes out as `BUY`, and everything downstream asks the venue about a contract that does
not exist.

Nothing else in the repository is affected — every other normalizer decodes the REST payloads,
whose field names are words (`symbol`, `commissionAsset`) with no case-only twin. The rule for
anything reading a WebSocket frame from this venue: **decode short keys through a
`map[string]json.RawMessage` and index them exactly.** A tagged struct is not safe here.

### Still unverified for M5

- The fifteen `incomeType` values the page refers to but does not render. Whitelisted rather
  than guessed (F16).
- Whether `/fapi/v3/positionRisk` returns every symbol or only symbols with an open position.
  The catalog page says "all symbols"; the endpoint page did not render far enough to confirm.
  The safe reading is "all", so a zero `positionAmt` row must be treated as *no position*
  rather than as a position of zero.
- The `ACCOUNT_UPDATE` payload's field names and its event-reason field. The stream is M5's
  live half; the REST snapshot above is enough to build and test the fold without it.

---

## 9. Reconciliation (M7)

Source: <https://developers.binance.com/docs/binance-spot-api-docs/rest-api/account-endpoints>
Fetched 2026-09-11, rendered.

### F21 — the spot account answers with its own instant, and the holding is `free + locked`

`GET /api/v3/account` — **IP weight 20**, signed. The response carries:

```json
{
  "updateTime": 123456789,
  "accountType": "SPOT",
  "balances": [ { "asset": "BTC", "free": "4723846.89208129", "locked": "0.00000000" } ],
  "permissions": ["SPOT"]
}
```

Two consequences, both structural rather than cosmetic:

1. **The holding is `free + locked`.** Comparing our fold against `free` alone would report
   every open order as a missing event — the balance sitting behind a resting limit order is
   held, not gone. This is the easiest available way to make reconciliation useless, and it
   fails in exactly the direction that looks like a real bug.

2. **`updateTime` is the exchange's own instant.** It means the snapshot does not have to be
   timestamped with our clock, and the difference between the two *is* the clock-skew check
   K14 asks for — obtained from a call we were making anyway, at no extra weight.

Weight 20 is the constraint on cadence. At one integration reconciling every five minutes
that is 240 weight an hour against a 6,000/minute IP budget (§3) — negligible; at 200
integrations on one IP it is not, which is why reconciliation runs at its own priority band
below realtime (`ARCHITECTURE.md` §7) rather than as fast as it can.

**Still unverified, and deferred rather than guessed:** whether `balances` omits zero
balances or includes them. Both readings are handled — an asset absent from their side and
an asset present at zero are treated identically — so the answer changes nothing, which is
why it is not worth a key to settle.
