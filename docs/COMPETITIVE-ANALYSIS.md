# Competitive Analysis — Crypto Portfolio & Risk Infrastructure

Date: 2026-09-01

---

## 1. The Market Splits Into Four Segments

### A) Retail trackers
**CoinStats, Delta (eToro), CoinMarketCap/CoinGecko portfolio, Crypto Pro, Kubera**

What they do: balance aggregation across many wallets and exchanges, price alerts,
mobile-first UI, NFTs, and in some cases a combined net-worth view alongside stocks
and ETFs.

What they don't: position-level accounting is absent or weak; the realized/unrealized
split is superficial; there are no risk metrics; derivatives support is recent and
shallow. CoinStats added the Hyperliquid, Aster and Lighter perp DEXs in early 2026 —
showing open positions, open orders and PnL — but that is *monitoring*, not risk
computation.

Their weaknesses are well documented by their own users: price identity errors, missing
tokens, totals that disagree between screens, and historical queries that break.

### B) Tax and accounting
**Koinly, CoinTracker, CoinTracking, CoinLedger, rotki (open source, AGPLv3)**

What they do: transaction-level ledgers, tax lot tracking, FIFO/LIFO/HIFO/ACB/share
pooling, cost basis, transfer matching, a "review suggested" queue for flagged records,
and jurisdiction-specific rule sets.

In the US, universal cost tracking was removed as of 1 January 2025; cost basis must now
be tracked per wallet or account. **That is an architectural constraint, not a tax
detail** — it means the ledger has to carry venue-scoped lots.

What they don't: they are not realtime (most lag by minutes to hours), they have no
derivative position risk, no concept of liquidation or margin, and no live exposure.

rotki is the closest open-source reference to us technically: local-first, an encrypted
local database via SQLCipher, EVM transaction decoding, period-based PnL reporting.
Worth studying — but it is Python plus Electron, and realtime risk is not among its
goals.

### C) On-chain / DeFi
**DeBank, Zapper, Zerion, Nansen Portfolio**

What they do: resolve protocol positions (lending, LP, staking, vaults) across many
chains, and bundle wallets together; manage airdrop and spam tokens.

What they don't: the CEX side is weak, there is no accounting, and there is no risk
engine.

### D) Institutional PMS / RMS
**1Token (CAM), Elwood, HedgeGuard, Talos, prime broker platforms**

This segment is the real architectural reference for what we are building. Their common
capability set:

| Capability | Detail |
|---|---|
| IBOR | Investment Book of Record — the single source feeding risk, NAV and reporting |
| Multi-entity | Fund, SPV, strategy sleeve and sub-account hierarchies |
| Multi-denomination | Portfolio denominated in BTC, ETH, USD or a stablecoin |
| Collateral & margin | Intra-venue leverage (MMR), extra-venue leverage (LTV), margin buffer |
| Risk | Realtime PnL, exposure, Greeks (delta/gamma/vega/theta) |
| Stress testing | Price, volatility and correlation shock scenarios; VaR; loss projection |
| Reconciliation | Equity and position reconciliation, **guided break resolution**, data integrity checks, anomaly detection |
| Alerting | PnL / exposure / leverage / collateral thresholds via Telegram, Slack, email, SMS |
| Lineage | Audit-ready portfolio history and data provenance |
| Backfill | Gapless historical data across every venue |

These products are closed, expensive and sold to fund operations teams. The leveraged
individual or professional trader cannot reach them.

---

## 2. Where the Gap Is

```
                 spot / balance        derivatives + risk
                 focused               focused
realtime    │  CoinStats, Delta   │      ← GAP →
            │  DeBank, Zapper     │   (1Token/Elwood, but priced out of reach)
────────────┼─────────────────────┼──────────────────────
historical /│  Koinly, CoinTracker│        —
accounting  │  CoinTracking, rotki│
```

**Conclusion: a derivative-aware, realtime, reconciled risk console for the leveraged
CEX trader does not exist.** Retail trackers show balances, tax tools keep history
correct, institutional platforms do both but are not accessible.

**The axis we will not compete on: integration count.** 1Token lists 72 exchanges and OTC
desks, 10 custodians, 163 chains and 4,224 DeFi protocols. Entering that race is
pointless. Differentiation will come from **accuracy and risk depth**, not coverage.

---

## 3. Where Competitors Lose Is Where We Get Tested

Failure modes drawn from the industry's own documentation:

1. **An identity error presents itself as a price error.**
   The user genuinely holds the asset and the quantity is right, but the holding is
   matched to the wrong market identity: a ticker collision, a wrapped asset, a symbol
   edge case. Even CCXT documents spot and derivative markets sharing a symbol as a known
   collision. Because the resulting number looks plausible, it gets debugged as a pricing
   problem — in the wrong place, for weeks.
   → Addressed by **K10** (asset ≠ instrument) and **K22** (time-scoped alias resolution).

2. **A transfer is mistaken for a sale.**
   When the receiving wallet is unknown, an outgoing transaction is treated as a
   disposal, and the user is asked to fix it by hand. This is the number-one support
   topic across the entire sector.
   → Addressed by **K12** (`transfer_links`, with an unmatched queue).

3. **Every screen shows a different total.**
   Different valuation sources are never reconciled, so the product feels like it is
   lying.
   → Addressed by **K11** (one valuation run per response) and **K23** (structured
   freshness).

4. **Time breaks the product.**
   Live prices are easy; historical accuracy is hard. Change the window and the PnL
   changes.
   → Addressed by **K1** (append-only ledger), **K2** (bitemporal time) and **K21**
   (canonical ordering).

5. **The product slows down as it grows.**
   Refresh loops and request logic fall apart under load.

Every one of these is a problem our event-sourced design is naturally suited to solve —
provided it is targeted explicitly rather than assumed.

---

## 4. Use Case: The Delta-Neutral Basis Trade

The target user's most common setup, and the thing existing tools get most wrong.

```
BTC spot long    +$50,000
BTC perp short   -$50,000
```

Naive computation: gross exposure $100k against $50k equity → **2× leverage, risky**.
Correct computation: net BTC delta ≈ 0 → no directional risk. The real risks are a
funding rate flip, liquidation of the short leg, basis risk, and venue risk.

A system that cannot tie these two legs to one strategy will alert the user constantly
and incorrectly — and a user who has learned to ignore alerts has no alerting at all.
**This is why the `strategy` dimension is in V1** (K13).

---

## 5. Gap Analysis — What the Original Draft Was Missing

In priority order. "V1" means it must be in the first release.

| # | Gap | Why it is critical | When | Decision |
|---|---|---|---|---|
| G1 | **Transfer matching** | A withdrawal plus a deposit across venues is one transfer; unmatched, it reads as a sale and PnL collapses | V1 intra-venue, M8 cross-venue | K12 |
| G2 | **Asset registry, separate from instrument** | Valuation errors are rooted in identity errors: ticker collisions, wrapped assets | V1 | K10, K22 |
| G3 | **A single valuation policy** | The "every screen shows a different total" problem; every response must carry the same `as_of` and price source | V1 | K11, K17 |
| G4 | **A data quality layer** | Negative balance, sequence gaps, unknown assets and price gaps are all missing-event alarms | V1 | K14 |
| G5 | **Strategy / sleeve dimension** | Delta-neutral setups are reported incorrectly without it | V1 | K13 |
| G6 | **Lineage / explain endpoint** | Free for us because we are event-sourced; it is what institutional products call "audit-ready" | V1 | K1 |
| G7 | **Alert channels + hysteresis** | Writing a row in a table is not an alert, and a threshold that oscillates must not spam | V1 | M6 / K51 |
| G8 | **Collateral as its own domain** | MMR (intra-venue) and LTV (extra-venue) are different concepts; leverage is not one number | M5 | M5 / K50 |
| G9 | **Reconciliation classification + resolution flow** | "Mismatch" alone is not actionable; a cause and an action are required | M7 | M7 / K53, K54, K55 |
| G10 | **Scenario shock** | "What if BTC drops 20%" — cheap because the engine is a pure function, and extremely valuable | M7.5 | M7.5 / K56 |
| G11 | **TWR / performance** | Deposits and withdrawals distort PnL; "returns change when the window changes" | V2 | — |
| G12 | **Tax lot projection** | US rules require per-wallet cost basis; the ledger must stay lot-derivable | V2 | K5 |
| G13 | **Reference currency + FX** | USD/USDT/BTC/TRY views; it matters during a stablecoin depeg | V1 decision, V2 multi-currency | K17 |
| G14 | **Sub-account structure** | Binance sub-accounts; several API keys under one user | V1 model, V2 UI | K15 |
| G15 | VaR, Greeks, options | Out of scope; Greeks are meaningless until options are supported | Out of scope | — |

---

## 6. Technology Note — CCXT

CCXT has a Go port (`github.com/ccxt/ccxt/go/v4`), but:

- WebSocket support lives in CCXT Pro and is paid for commercial use
- The Go port is transpiled, not idiomatic Go
- Account and user-data-stream normalization **is this project's core domain work** —
  outsource it and what remains is CRUD

**Decision: write the exchange clients by hand.** But read CCXT's market metadata and
symbol normalization model as a reference; they have already solved problems like the
spot/derivative symbol collision.

---

## 7. Product Position (one sentence)

> For the leveraged CEX trader: a canonical, reconciled, strategy-aware realtime risk
> engine built on an append-only ledger.

What sells is an accuracy claim, not a list of competitors. That is why the most visible
feature of this product must not be how many exchanges it supports, but
**"I can show you why my numbers are correct"**: reconciliation status, the data quality
panel, and the traceability of every figure down to its events are first-class citizens
in the interface.
