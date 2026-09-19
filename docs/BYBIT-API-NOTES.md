# Bybit V5 — verified venue notes

The same rule as `BINANCE-API-NOTES.md`: every fact here was read from the official
documentation and is dated. Nothing in this file comes from memory or from a blog post
(`CLAUDE.md` §2). A fact that could not be verified is written down as unverified rather than
guessed, because a plausible guess about a venue produces plausible, wrong numbers.

Read 2026-09-11. Findings are numbered **B1, B2, …** so they cannot be confused with the
Binance findings (F1–F21).

| Subject | Source |
|---|---|
| Authentication, base URL | <https://bybit-exchange.github.io/docs/v5/guide> |
| Deposit records | <https://bybit-exchange.github.io/docs/v5/asset/deposit/deposit-record> |
| Withdrawal records | <https://bybit-exchange.github.io/docs/v5/asset/withdraw/withdraw-record> |
| Status enums | <https://bybit-exchange.github.io/docs/v5/enum> |
| API key permissions | <https://bybit-exchange.github.io/docs/v5/user/apikey-info> |

---

## B1 — The signature is over a concatenation, not a query string

Mainnet is `https://api.bybit.com` (`https://api.bytick.com` is the documented alternate).

A signed request carries four headers:

```
X-BAPI-API-KEY
X-BAPI-TIMESTAMP        UTC milliseconds
X-BAPI-SIGN
X-BAPI-RECV-WINDOW      optional, default 5000 ms
```

and the signed payload is:

- **GET:** `timestamp + api_key + recv_window + queryString`
- **POST:** `timestamp + api_key + recv_window + jsonBodyString`

HMAC-SHA256, lower-case hex (RSA-SHA256 base64 is the documented alternative).

This is **not** Binance's scheme, and the difference is the kind that produces a working
signature for the wrong string. Binance signs the query string alone and passes the result as
a `signature` parameter; Bybit signs a concatenation that begins with the timestamp and the
key, and passes it in a header. A client written by analogy to the Binance one authenticates
nothing.

---

## B2 — Bybit publishes the two enums Binance does not, so its withdrawals ARE normalizable

This is the finding that shapes M8.

`DepositStatus`, quoted from the enum page:

```
0 - unknown
1 - toBeConfirmed
2 - processing
3 - success
4 - deposit failed
7 - Rollback processing
70011 - Deposit rolled back
70012 - Deposit remains successful after rollback review
70013 - Deposit rollback faill
10011 - pending to be credited to funding pool
10012 - Credited to funding pool successfully
```

`WithdrawStatus` is a **string** enum, quoted:

```
SecurityCheck · Pending · success · CancelByUser · Reject · Fail · BlockchainConfirmed
MoreInformationRequired · Unknown · HighValueReviewPending · HighValueReviewEDDSubmission
HighValueReviewRejected · HighValueReviewRejectedRfunding · HighValueReviewRejectedRefunded
```

Two consequences:

1. **`NormalizeBybitWithdrawal` can exist**, where `NormalizeWithdrawal` for Binance cannot
   (K34): Binance publishes only the garbled fragment `0(0 Sent, 2 Approval 3 4 6)`, re-checked
   on 2026-09-11 and still not enumerated. Which code means "the coins left" decides whether a
   balance is right, and encoding a remembered enum into append-only financial rows is exactly
   what is forbidden.

2. **The enums are still closed sets we must refuse outside of.** `70012` reads as a success
   *after a rollback review* and `70011` as a rollback — two codes whose difference is the whole
   answer. The normalizer accepts only what this list names and rejects anything else loudly,
   because a new code mapped to the nearest plausible meaning is the failure mode this rule
   exists to prevent.

---

## B3 — Thirty-day windows, fifty rows, and a cursor

Both endpoints:

| | |
|---|---|
| Deposits | `GET /v5/asset/deposit/query-record` |
| Withdrawals | `GET /v5/asset/withdraw/query-record` |
| Window | `endTime - startTime` **must be under 30 days**; unspecified means the last 30 days |
| Page size | `[1, 50]`, default 50 |
| Paging | `nextPageCursor`, not an offset |

Thirty days against Binance's ninety-day wallet windows (deposits/withdrawals) and the
seven-day/three-month futures horizons (userTrades/income, F19), so
the two venues' walks are not the same shape and the backfill scope vocabulary has to carry
both. Fifty rows a page is small: an account with a busy deposit history pages a great many
times, which is a rate-limit question before it is a correctness one.

`txID` cannot be queried for transactions generated before 2024-01-01 — a limit on lookup, not
on the walk.

---

## B4 — Times are epoch milliseconds, in strings

`createTime` and `updateTime` on a withdrawal arrive as `"1742738305000"`: epoch milliseconds,
quoted. `successAt` on a deposit is milliseconds likewise.

This is the one place Bybit is *easier* than Binance, and it is worth stating because the
Binance withdrawal blocker is half a timezone problem (K34): `applyTime` there is
`"2019-10-12 11:12:02"` with no timezone stated, and an eight-hour error corrupts the canonical
order (L7) and every time-windowed reconciliation. Bybit has no such ambiguity.

The deposit page adds one caveat, quoted: the timestamps are milliseconds "though the query
logic is actually effective based on **second** level". So a window boundary is a second, not
a millisecond, and a walk that assumed otherwise would re-read or skip a boundary row. The
walk overlaps its windows by one second rather than assuming which way it rounds.

---

## B5 — the key says whether it is read-only, in one field

`GET /v5/user/query-api`, read 2026-09-11 from
<https://bybit-exchange.github.io/docs/v5/user/apikey-info>. Documented as accessible "with
any permission", so a read-only key can ask about itself.

The decisive field:

```
readOnly   0 = read/write   1 = read-only
```

and beside it a `permissions` object of named arrays:

```
ContractTrade: ["Order", "Position"]      Spot:    ["SpotTrade"]
Wallet:        ["AccountTransfer", "SubMemberTransfer", "Withdraw"]
Options:       ["OptionsTrade"]           Derivatives: ["DerivativesTrade"]
Exchange:      ["ExchangeHistory"]        Earn, FiatP2P, Affiliate, BlockTrade, ...
```

**This is cleaner than Binance's, and the difference is worth naming.** F8 had to infer
read-only status from a set of booleans whose meanings the page never states; here the venue
says it outright. A key is accepted only when `readOnly == 1` **and** every permission string
it holds is on a closed allowlist -- two independent checks, either of which rejects, because
`readOnly` is the venue's promise and the allowlist is ours (K9, L13).

The allowlist is closed rather than a denylist for the same reason Binance's is: a venue that
adds a permission would have it accepted by default, and a permission we have never heard of
is rejected until someone decides otherwise.

**Still unverified:** whether reading deposit and withdrawal records *requires* a granted
permission at all, or whether `readOnly` alone suffices. The page for those endpoints states
no permission. A key that turns out to need one fails loudly at the first call with the
venue's own error, which is the right failure -- the alternative is inferring a permission
model and being wrong quietly.

---

## Unverified, and therefore not built

- **The rate limit for these two endpoints.** The pages do not state one. The shared limiter
  (K24) governs them at the IP level either way, but the per-endpoint cost is unknown, so the
  walk is paced conservatively rather than tuned.
- **Everything about Bybit trades, positions and funding.** M8 covers transfers across venues,
  not a second full ingest. A Bybit portfolio is a later milestone, and pretending otherwise by
  half-building it is how a venue ends up with a normalizer nothing calls (K38, twice).
