# Plimsoll

Crypto portfolio and risk infrastructure for a leveraged CEX trader: a canonical,
reconciled, strategy-aware risk engine built on an append-only ledger. The aim is
that the numbers are right and can be traced down to the events that produced them.

Plimsoll reads. It never trades, and it rejects API keys that are not read-only.

## Status

Milestones M0 to M8 are implemented (see [docs/PROJECT.md](docs/PROJECT.md) §8):
tenancy, ledger and position engine, Binance spot ingestion, portfolio API with
lineage, data quality, market data and valuation, perpetuals and collateral,
strategy tagging, risk and alerting, reconciliation, scenario shock, and Bybit
cross-venue transfers.

**M2 is not verified against a real account.** The Binance backfill is code
complete and tested against recorded fixtures and a fake exchange, but no real
read-only key has been connected. Real account history, idempotency on real
payloads and rate-limit behavior are unproven. Bybit trades, positions and funding
are not built by decision.

## Run it

Requires Docker, Go and Node. Copy `.env.example` to `.env`, then:

```
make up                 # build, migrate and start the stack
make test               # unit tests, no Docker
make test-integration   # real Postgres via compose
make lint
make frontend-check
make down
```

## Documentation

- [docs/PROJECT.md](docs/PROJECT.md): scope, canonical model, API, milestones
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): modules, data flow, tenancy, workers
- [docs/DECISIONS.md](docs/DECISIONS.md): the decision register (K1 to K57)
- [docs/COMPETITIVE-ANALYSIS.md](docs/COMPETITIVE-ANALYSIS.md): positioning
- [docs/BINANCE-API-NOTES.md](docs/BINANCE-API-NOTES.md) and [docs/BYBIT-API-NOTES.md](docs/BYBIT-API-NOTES.md): verified venue facts
- [CLAUDE.md](CLAUDE.md): working rules and invariants (identical to `AGENTS.md`)
