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
)

// Endpoint weights, each verified against the official documentation on 2026-09-04 and
// recorded in docs/BINANCE-API-NOTES.md section 2. They are constants rather than literals
// at the call site so a reviewer can check every number against the source without leaving
// this file -- a wrong weight produces a plausible client and an IP ban.
const (
	weightExchangeInfo = 20 // GET /api/v3/exchangeInfo
	weightAccount      = 20 // GET /api/v3/account
	weightMyTrades     = 20 // GET /api/v3/myTrades without orderId
	weightMyTradesByID = 5  // GET /api/v3/myTrades with orderId
	weightDeposits     = 1  // GET /sapi/v1/capital/deposit/hisrec
	weightWithdrawals  = 1  // GET /sapi/v1/capital/withdraw/history
	weightRestrictions = 1  // GET /sapi/v1/account/apiRestrictions
	weightTransfers    = 1  // GET /sapi/v1/asset/transfer (IP)
)

// MaxTradeWindow is the widest startTime..endTime span myTrades will answer, quoted from
// rest-api.md on 2026-09-04: "The time between startTime and endTime can't be longer than
// 24 hours." It lives here rather than at the callers because it is a fact about the venue,
// and two callers holding their own copy of it is two places for it to drift.
const MaxTradeWindow = 24 * time.Hour

// ErrUnsupportedQuery means the parameters asked for a combination the endpoint does not
// accept. Refused before the request is sent, because the alternative is a rejection the
// caller reads as "no trades" -- and in the gap-resync path that is a window silently left
// unfilled (L11).
var ErrUnsupportedQuery = errors.New("binance: unsupported parameter combination")

// ErrNoRequestWeightLimit means exchangeInfo did not state a one-minute REQUEST_WEIGHT
// ceiling. It is an error rather than a fallback because every fallback is a number that
// will be wrong the day Binance changes theirs, and the way we would find out is the ban
// (K24).
var ErrNoRequestWeightLimit = errors.New(
	"binance: exchangeInfo has no 1-minute REQUEST_WEIGHT limit")

// ExchangeInfo returns trading rules and, more importantly here, the rateLimits array the
// limiter's ceiling is read from. Unsigned: it carries no account data, and signing it
// would mint a signature on every connect for nothing.
func (c *Client) ExchangeInfo(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, request{path: "/api/v3/exchangeInfo", weight: weightExchangeInfo})
}

// Account returns balances and permissions for the key. Signed.
func (c *Client) Account(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, request{path: "/api/v3/account", weight: weightAccount, signed: true})
}

// APIRestrictions returns the key's permission flags. It is what integration.Verify reads
// to refuse anything that can do more than read (K9).
func (c *Client) APIRestrictions(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, request{
		path: "/sapi/v1/account/apiRestrictions", weight: weightRestrictions, signed: true,
	})
}

// MyTradesQuery is one page of spot trade history. Symbol is required -- there is no
// "all my trades" endpoint (F4), which is why discovery has to find the symbols first.
type MyTradesQuery struct {
	Symbol string
	// FromID walks by trade id: trades with id >= FromID are returned. Nil means the most
	// recent page. This is the strategy F5 rests on and it is not yet verified against a
	// real account -- see docs/BINANCE-API-NOTES.md section 5.
	FromID *int64
	// OrderID narrows to one order and drops the weight from 20 to 5.
	OrderID *int64
	// StartTime and EndTime bound the page. Binance rejects a span over 24 hours, so a
	// historical walk by time is a walk in 24-hour chunks (F5).
	StartTime, EndTime time.Time
	// Limit is capped at 1000 by the exchange; zero leaves Binance's default of 500.
	Limit int
}

// validate refuses combinations the documentation does not list.
//
// rest-api.md enumerates them, verbatim on 2026-09-04: "symbol; symbol + orderId;
// symbol + startTime; symbol + endTime; symbol + fromId; symbol + startTime + endTime;
// symbol + orderId + fromId." A time range with fromId is absent from that list, so a
// historical walk by id and a walk by window are two strategies rather than one with an
// optional parameter -- the same trap userTrades states outright for futures.
func (q MyTradesQuery) validate() error {
	hasWindow := !q.StartTime.IsZero() || !q.EndTime.IsZero()
	if q.FromID != nil && hasWindow {
		return fmt.Errorf(
			"%w: myTrades takes fromId or a time range, never both", ErrUnsupportedQuery)
	}
	if !q.StartTime.IsZero() && !q.EndTime.IsZero() {
		if span := q.EndTime.Sub(q.StartTime); span > MaxTradeWindow {
			return fmt.Errorf("%w: %s..%s spans %s, and the venue answers at most 24h",
				ErrUnsupportedQuery,
				q.StartTime.UTC().Format(time.RFC3339), q.EndTime.UTC().Format(time.RFC3339),
				span)
		}
	}
	return nil
}

// MyTrades returns one page of spot trades for a symbol.
func (c *Client) MyTrades(ctx context.Context, q MyTradesQuery) (json.RawMessage, error) {
	if q.Symbol == "" {
		return nil, fmt.Errorf("binance: myTrades needs a symbol; there is no all-trades endpoint")
	}
	if err := q.validate(); err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("symbol", q.Symbol)
	setInt64(query, "fromId", q.FromID)
	setInt64(query, "orderId", q.OrderID)
	setTime(query, "startTime", q.StartTime)
	setTime(query, "endTime", q.EndTime)
	if q.Limit > 0 {
		query.Set("limit", strconv.Itoa(q.Limit))
	}

	weight := weightMyTrades
	if q.OrderID != nil {
		weight = weightMyTradesByID
	}
	return c.do(ctx, request{
		path: "/api/v3/myTrades", query: query, weight: weight, signed: true,
	})
}

// HistoryQuery is the shared shape of the deposit and withdrawal endpoints, which page by
// offset rather than by id and cap their span at 90 days.
type HistoryQuery struct {
	StartTime, EndTime time.Time
	Offset, Limit      int
}

// DepositHistory returns deposits. A deposit is one half of a transfer, and matching it to
// the withdrawal on the other side is what K12 does later.
func (c *Client) DepositHistory(ctx context.Context, q HistoryQuery) (json.RawMessage, error) {
	return c.do(ctx, request{
		path:   "/sapi/v1/capital/deposit/hisrec",
		query:  q.values(),
		weight: weightDeposits,
		signed: true,
	})
}

// TransferQuery is the universal-transfer endpoint's shape, which is not HistoryQuery's.
//
// Type is REQUIRED and is a direction rather than a category (F10), so "every transfer" is
// one call per direction. Paging is `current` (1-based) and `size` (max 100) rather than an
// offset and a limit -- a different spelling of the same weak, shifting-page mechanism, and
// the reason the walk pins a time window around it.
type TransferQuery struct {
	Type               string
	StartTime, EndTime time.Time
	Page, Size         int
}

// TransferPageSize is the maximum `size` the endpoint documents. Exported because the walk
// pages against it and a walk that guessed a larger number would silently read short pages.
const TransferPageSize = 100

// UniversalTransferHistory returns one page of one direction's transfers. Weight 1 (IP).
//
// The response is an object -- {"total": n, "rows": [...]} -- not the bare array the
// deposit endpoints return, which is why it has its own decoder rather than sharing one.
func (c *Client) UniversalTransferHistory(
	ctx context.Context, q TransferQuery,
) (json.RawMessage, error) {
	return c.do(ctx, request{
		path:   "/sapi/v1/asset/transfer",
		query:  q.values(),
		weight: weightTransfers,
		signed: true,
	})
}

func (q TransferQuery) values() url.Values {
	query := url.Values{}
	query.Set("type", q.Type)
	setTime(query, "startTime", q.StartTime)
	setTime(query, "endTime", q.EndTime)
	if q.Page > 0 {
		query.Set("current", strconv.Itoa(q.Page))
	}
	if q.Size > 0 {
		query.Set("size", strconv.Itoa(q.Size))
	}
	return query
}

// WithdrawHistory returns withdrawals.
func (c *Client) WithdrawHistory(ctx context.Context, q HistoryQuery) (json.RawMessage, error) {
	return c.do(ctx, request{
		path:   "/sapi/v1/capital/withdraw/history",
		query:  q.values(),
		weight: weightWithdrawals,
		signed: true,
	})
}

func (q HistoryQuery) values() url.Values {
	query := url.Values{}
	setTime(query, "startTime", q.StartTime)
	setTime(query, "endTime", q.EndTime)
	if q.Offset > 0 {
		query.Set("offset", strconv.Itoa(q.Offset))
	}
	if q.Limit > 0 {
		query.Set("limit", strconv.Itoa(q.Limit))
	}
	return query
}

// SpotSymbols lists every symbol exchangeInfo names, sorted and deduplicated. It is what
// discovery sweeps: there is no endpoint returning spot trades across symbols (F4), so the
// symbols have to be enumerated before any of them can be probed.
//
// No status filter. A symbol that is halted, broken, or no longer trading can still hold
// the fills that acquired an asset, and a discovery that skipped it would leave exactly the
// missing acquisition K26 refuses. What this cannot recover is a pair delisted outright:
// exchangeInfo stops naming it, so it can no longer be probed -- a gap in what the exchange
// will tell us, and one that belongs in freshness rather than in an assumption.
func SpotSymbols(raw json.RawMessage) ([]string, error) {
	var payload struct {
		Symbols []struct {
			Symbol string `json:"symbol"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("binance: exchangeInfo has no symbols array: %w", err)
	}

	seen := make(map[string]bool, len(payload.Symbols))
	out := make([]string, 0, len(payload.Symbols))
	for i, entry := range payload.Symbols {
		if entry.Symbol == "" {
			return nil, fmt.Errorf(
				"binance: exchangeInfo symbol %d has no name; a sweep over a list with holes"+
					" in it reports a complete discovery it did not do", i)
		}
		if seen[entry.Symbol] {
			continue
		}
		seen[entry.Symbol] = true
		out = append(out, entry.Symbol)
	}
	sort.Strings(out)
	return out, nil
}

// rateLimitEntry is one row of exchangeInfo's rateLimits array.
type rateLimitEntry struct {
	RateLimitType string `json:"rateLimitType"`
	Interval      string `json:"interval"`
	IntervalNum   int    `json:"intervalNum"`
	Limit         int    `json:"limit"`
}

// RequestWeightPerMinute reads the IP weight ceiling out of an exchangeInfo payload. It
// accepts only a ceiling stated over exactly one minute: a REQUEST_WEIGHT limit expressed
// over five minutes is not five times a one-minute budget, and dividing it down would
// produce a number that is wrong in the direction that gets the IP banned.
func RequestWeightPerMinute(raw json.RawMessage) (int, error) {
	var payload struct {
		RateLimits []rateLimitEntry `json:"rateLimits"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrNoRequestWeightLimit, err)
	}
	for _, entry := range payload.RateLimits {
		if entry.RateLimitType != "REQUEST_WEIGHT" || entry.Interval != "MINUTE" {
			continue
		}
		if entry.IntervalNum != 1 || entry.Limit <= 0 {
			continue
		}
		return entry.Limit, nil
	}
	return 0, ErrNoRequestWeightLimit
}

func setInt64(query url.Values, key string, value *int64) {
	if value != nil {
		query.Set(key, strconv.FormatInt(*value, 10))
	}
}

func setTime(query url.Values, key string, value time.Time) {
	if !value.IsZero() {
		query.Set(key, strconv.FormatInt(value.UnixMilli(), 10))
	}
}
