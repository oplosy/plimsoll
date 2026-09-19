package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"
)

// weightFuturesExchangeInfo is GET /fapi/v1/exchangeInfo, verified on 2026-09-10 (F14's
// sibling on the market-data page). Weight 1 (IP).
const weightFuturesExchangeInfo = 1

// FuturesBaseURL is the USD-M REST host. Separate from the spot host because they are
// separate services, not two paths on one server.
const FuturesBaseURL = "https://fapi.binance.com"

// contractPerpetual is the only contractType this project models.
//
// The four documented values are PERPETUAL, CURRENT_QUARTER, NEXT_QUARTER and
// TRADIFI_PERPETUAL. A quarterly contract DELIVERS: it closes itself on a date, and folded
// as a perpetual it stays open forever, leaving a phantom exposure the user cannot close
// because it does not exist. TRADIFI_PERPETUAL is perpetual but its underlying is an index
// rather than a coin, which the asset registry has no entry for -- and inventing one is how
// a position acquires an asset nobody can price.
const contractPerpetual = "PERPETUAL"

// statusTrading is the only status a contract is swept in. The page shows TRADING as its
// example and does not enumerate the rest, so this is a whitelist for the same reason F11's
// is: opening a walk for a settling contract spends weight on history that is closing, and
// a status this code has never seen should stop it rather than be assumed benign.
const statusTrading = "TRADING"

// IsModelledContract reports whether a contractType is one this project folds. Exported so
// the sweep and any future reconciliation check ask the same question of the same table.
func IsModelledContract(contractType string) bool {
	return contractType == contractPerpetual
}

// FuturesContract is one USD-M contract as the registry needs it: the venue's symbol and
// the three assets that decide what a fill and a funding payment move.
//
// MarginAsset is what the contract settles in, which for a USD-M perp is the quote asset
// and for a coin-margined one would not be. It is carried explicitly rather than inferred,
// because inferring it is right for every contract V1 trades and wrong for the first one it
// does not -- correct in testing, wrong in production.
type FuturesContract struct {
	Symbol      string
	BaseAsset   string
	QuoteAsset  string
	MarginAsset string
}

// FuturesContracts reads the perpetuals out of a USD-M exchangeInfo payload, sorted.
//
// It filters rather than returns the list: the futures universe includes quarterly futures
// and index perpetuals, and a sweep that opened a walk for each of them would be walking
// contracts this ledger cannot fold.
func FuturesContracts(raw json.RawMessage) ([]FuturesContract, error) {
	var payload struct {
		Symbols []struct {
			Symbol       string `json:"symbol"`
			ContractType string `json:"contractType"`
			Status       string `json:"status"`
			BaseAsset    string `json:"baseAsset"`
			QuoteAsset   string `json:"quoteAsset"`
			MarginAsset  string `json:"marginAsset"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("binance: futures exchangeInfo has no symbols array: %w", err)
	}

	out := make([]FuturesContract, 0, len(payload.Symbols))
	seen := make(map[string]bool, len(payload.Symbols))
	for i, entry := range payload.Symbols {
		if entry.Symbol == "" {
			// The same refusal SpotSymbols makes: a sweep over a list with holes in it
			// reports a complete discovery it did not do (K33).
			return nil, fmt.Errorf(
				"binance: futures exchangeInfo symbol %d has no name; a sweep over a list"+
					" with holes in it reports a complete discovery it did not do", i)
		}
		if !IsModelledContract(entry.ContractType) || entry.Status != statusTrading {
			continue
		}
		if entry.MarginAsset == "" {
			return nil, fmt.Errorf(
				"binance: perpetual %s names no margin asset, and a perp that does not say"+
					" what it settles in cannot be stored (00005)", entry.Symbol)
		}
		if seen[entry.Symbol] {
			continue
		}
		seen[entry.Symbol] = true
		out = append(out, FuturesContract{
			Symbol:      entry.Symbol,
			BaseAsset:   entry.BaseAsset,
			QuoteAsset:  entry.QuoteAsset,
			MarginAsset: entry.MarginAsset,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

// FuturesExchangeInfo fetches the USD-M contract universe. Unsigned: it is public data, and
// asking for it with a key attached would put an account's identity on a request that does
// not need one (F6's rule, applied to a second endpoint).
func (c *Client) FuturesExchangeInfo(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, request{
		path:    "/fapi/v1/exchangeInfo",
		weight:  weightFuturesExchangeInfo,
		futures: true,
	})
}

// ErrHedgeMode means the account is in hedge mode, which V1 does not model.
//
// A hedge-mode account reports LONG and SHORT rows for one symbol; this fold keeps one
// position per instrument, so folding both sides into it averages a long and a short
// together and reports a position that is FLAT while the account carries two live exposures.
// The refusal is loud because the alternative is an import that half works: the numbers
// would be there, plausible, and describing an account that does not exist.
var ErrHedgeMode = errors.New("binance: hedge mode is not modelled in V1 (one-way only)")

// positionSideOneWay is what the venue reports for every position in one-way mode.
const positionSideOneWay = "BOTH"

// futuresTradeRow is one element of a userTrades response (F18). Every money field stays a
// string until it reaches decimal (L1).
type futuresTradeRow struct {
	Symbol string `json:"symbol"`
	ID     int64  `json:"id"`

	// Side is the authoritative direction. `buyer` says the same thing today and is a
	// derived field; reading it instead would rest on that agreement, which is exactly the
	// reading that breaks quietly when a venue changes what it derives.
	Side  string `json:"side"`
	Price string `json:"price"`
	Qty   string `json:"qty"`

	Commission      string `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`

	// RealizedPnl is read off the wire and deliberately never stored on the event. The
	// position engine computes realized PnL from the average-cost fold (K5); keeping the
	// venue's copy too would be two numbers for one fact, and the fold would either
	// disagree with it or be replaced by it -- the second source of truth L3 forbids. It
	// survives in raw forever (L15), which is what makes it M7's reconciliation input.
	RealizedPnl string `json:"realizedPnl"`

	PositionSide string `json:"positionSide"`
	Time         int64  `json:"time"`
}

// NormalizeFuturesTrade turns one USD-M userTrades row into a canonical event.
//
// It resolves in the USD-M market, never in whichever market a caller passes: spot BTCUSDT
// and perp BTCUSDT are the same string and different instruments, and resolving a perp fill
// against the spot alias attaches a leveraged position's quantity to a spot one (K10, L8).
//
// raw is stored verbatim (L15).
func NormalizeFuturesTrade(
	ctx context.Context, r Resolver, ic IngestContext, raw json.RawMessage,
) (ledger.Event, error) {
	var row futuresTradeRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return ledger.Event{}, fmt.Errorf("%w: decode userTrades element: %w", ErrMalformedTrade, err)
	}

	if row.PositionSide != "" && row.PositionSide != positionSideOneWay {
		return ledger.Event{}, fmt.Errorf("%w: trade %d on %s reports positionSide %q",
			ErrHedgeMode, row.ID, row.Symbol, row.PositionSide)
	}
	if row.ID == 0 {
		return ledger.Event{}, fmt.Errorf(
			"%w: futures fill has no trade id, so it has no identity", ErrMalformedTrade)
	}
	if row.Symbol == "" {
		return ledger.Event{}, fmt.Errorf("%w: futures fill %d names no symbol",
			ErrMalformedTrade, row.ID)
	}
	if row.Time <= 0 {
		return ledger.Event{}, fmt.Errorf("%w: futures fill %d has no exchange timestamp",
			ErrMalformedTrade, row.ID)
	}

	var side ledger.Side
	switch row.Side {
	case "BUY":
		side = ledger.SideBuy
	case "SELL":
		side = ledger.SideSell
	default:
		return ledger.Event{}, fmt.Errorf("%w: futures fill %d has side %q",
			ErrMalformedTrade, row.ID, row.Side)
	}

	quantity, err := parseAmount(row.Qty)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("%w: futures fill %d quantity: %w",
			ErrMalformedTrade, row.ID, err)
	}
	price, err := parseAmount(row.Price)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("%w: futures fill %d price: %w",
			ErrMalformedTrade, row.ID, err)
	}
	if !quantity.IsPositive() || !price.IsPositive() {
		// The side carries the direction; the numbers carry the size. A signed quantity
		// here would be a second statement of the direction, free to disagree with it.
		return ledger.Event{}, fmt.Errorf("%w: futures fill %d is %s at %s",
			ErrMalformedTrade, row.ID, quantity, price)
	}

	eventTime := time.UnixMilli(row.Time).UTC()
	instrumentID, err := r.Instrument(ctx, instrument.MarketUSDM, row.Symbol, eventTime)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("normalize usdm trade %d (%s): %w",
			row.ID, row.Symbol, err)
	}

	fee, feeAsset, feeAssetID, err := resolveFee(ctx, r, row.Commission, row.CommissionAsset, eventTime)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("normalize usdm trade %d fee: %w", row.ID, err)
	}

	return ledger.Event{
		AccountID:     ic.AccountID,
		IntegrationID: ic.IntegrationID,

		VenueEventID:  FuturesTradeID(row.Symbol, row.ID),
		VenueSequence: row.ID,
		Source:        ic.Source,

		EventType:    ledger.TypeTrade,
		InstrumentID: &instrumentID,
		Side:         side,

		Quantity: decimal.NullDecimal{Decimal: quantity, Valid: true},
		Price:    decimal.NullDecimal{Decimal: price, Valid: true},

		Fee:        fee,
		FeeAsset:   feeAsset,
		FeeAssetID: feeAssetID,

		EventTime: eventTime,
		Raw:       raw,
	}, nil
}

// FuturesTradeID is the canonical identity of a USD-M fill. The market prefix is what keeps
// it from colliding with the spot fill of the same ticker and id (K19, L5).
func FuturesTradeID(symbol string, tradeID int64) string {
	return fmt.Sprintf("usdm:trade:%s:%d", symbol, tradeID)
}

// The USD-M account endpoints, with the weights verified on 2026-09-10 (F14, F15).
const (
	weightFuturesAccount  = 5 // GET /fapi/v3/account
	weightPositionRisk    = 1 // GET /fapi/v3/positionRisk
	weightLeverageBracket = 1 // GET /fapi/v1/leverageBracket
)

// FuturesAccount returns the account's margin totals, including the maintenance requirement
// -- which positionRisk does NOT carry (F14). This is the endpoint the margin buffer comes
// from.
func (c *Client) FuturesAccount(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, request{
		path:    "/fapi/v3/account",
		weight:  weightFuturesAccount,
		signed:  true,
		futures: true,
	})
}

// PositionRisk returns every position's mark, notional and liquidation price. The
// liquidation price is read and never computed (K6).
func (c *Client) PositionRisk(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, request{
		path:    "/fapi/v3/positionRisk",
		weight:  weightPositionRisk,
		signed:  true,
		futures: true,
	})
}

// LeverageBracket returns the maintenance-margin tier table. Signed because a user's
// brackets depend on their own tier, so this is account data rather than market data.
func (c *Client) LeverageBracket(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, request{
		path:    "/fapi/v1/leverageBracket",
		weight:  weightLeverageBracket,
		signed:  true,
		futures: true,
	})
}

// The two history endpoints the futures ledger is filled from, and their documented weights.
const (
	weightFuturesUserTrades = 5  // GET /fapi/v1/userTrades
	weightIncome            = 30 // GET /fapi/v1/income
)

// FuturesHistory is how far back either endpoint will answer. Quoted from the pages:
// userTrades "Only support querying trade in the past 3 months", and income history "only
// contains data for the last three months".
//
// It is a permanent horizon rather than a walk that has not finished, which is the
// difference between history_truncated and backfill_incomplete: telling a user to wait for
// something that will not arrive is the confident-and-wrong failure L11 exists to reject.
const FuturesHistory = 90 * 24 * time.Hour

// FuturesWindow is the widest range either endpoint accepts in one request: "The time
// between startTime and endTime cannot be longer than 7 days."
const FuturesWindow = 7 * 24 * time.Hour

// FuturesTradesQuery is one page of one symbol's futures fills.
//
// Time-windowed rather than id-paged, unlike spot. Spot pages by trade id because its
// history reaches back to 2017 and a time walk would be thousands of requests per symbol --
// which forced an inference about what fromId=0 returns (F5) that has to be checked at
// runtime. Here the venue answers for three months at most, so thirteen windows cover the
// whole of what exists and nothing has to be inferred at all.
type FuturesTradesQuery struct {
	Symbol             string
	StartTime, EndTime time.Time
	Limit              int
}

// FuturesUserTrades returns one page of one symbol's fills. Weight 5 (IP), signed.
func (c *Client) FuturesUserTrades(
	ctx context.Context, q FuturesTradesQuery,
) (json.RawMessage, error) {
	return c.do(ctx, request{
		path:    "/fapi/v1/userTrades",
		query:   q.values(),
		weight:  weightFuturesUserTrades,
		signed:  true,
		futures: true,
	})
}

func (q FuturesTradesQuery) values() url.Values {
	query := url.Values{}
	query.Set("symbol", q.Symbol)
	setTime(query, "startTime", q.StartTime)
	setTime(query, "endTime", q.EndTime)
	if q.Limit > 0 {
		query.Set("limit", strconv.Itoa(q.Limit))
	}
	return query
}

// IncomeQuery is one page of the income history: funding, and the three types the fold
// deliberately refuses (F16).
type IncomeQuery struct {
	StartTime, EndTime time.Time

	// Page is 1-based. The endpoint offers it alongside limit, and a window with more rows
	// than one page holds is not hypothetical: a busy account pays funding on every open
	// position every eight hours.
	Page  int
	Limit int
}

// IncomeHistory returns one page of the account's cash flows. Weight 30 (IP), signed.
//
// Thirty is the most expensive request in this system by a factor of six, which is why the
// walk asks for the largest page the endpoint allows rather than the most convenient one.
func (c *Client) IncomeHistory(ctx context.Context, q IncomeQuery) (json.RawMessage, error) {
	return c.do(ctx, request{
		path:    "/fapi/v1/income",
		query:   q.values(),
		weight:  weightIncome,
		signed:  true,
		futures: true,
	})
}

func (q IncomeQuery) values() url.Values {
	query := url.Values{}
	setTime(query, "startTime", q.StartTime)
	setTime(query, "endTime", q.EndTime)
	if q.Page > 0 {
		query.Set("page", strconv.Itoa(q.Page))
	}
	if q.Limit > 0 {
		query.Set("limit", strconv.Itoa(q.Limit))
	}
	return query
}

// FuturesPageSize is the maximum `limit` both endpoints document.
const FuturesPageSize = 1000
