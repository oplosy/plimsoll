package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/asset"
	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Where an event was first seen. Metadata only: it is never part of venue_event_id, because
// REST backfill and the stream report the same trade and an identity including the source
// would insert both and double the position (K19, L5).
const (
	SourceREST   = "binance-rest"
	SourceStream = "binance-ws"
)

var (
	// ErrNotAFill means the message was a real, understood event that simply does not move
	// a position -- an order placed, cancelled, rejected or expired. The caller skips it.
	// Deliberately distinct from an error: skipping is correct here and wrong everywhere
	// else in this file.
	ErrNotAFill = errors.New("binance: execution report is not a fill")

	// ErrUnknownExecutionType means Binance reported an execution type we do not model.
	// It is loud on purpose. Treating it as a non-fill would silently drop a trade the day
	// Binance introduces a new way to report one, and a missing acquisition poisons the
	// cost basis permanently (K26).
	ErrUnknownExecutionType = errors.New("binance: unknown execution type")

	// ErrMalformedTrade means the payload cannot be a fill whatever it claims -- no trade
	// id, no quantity, an unreadable side. The caller raises a data-quality finding (K14)
	// rather than writing an event that the fold cannot place.
	ErrMalformedTrade = errors.New("binance: trade payload is not usable")
)

// InstrumentResolver is the slice of the registries a trade needs. Declared as an interface
// here so normalization stays a unit test with no Docker: the question these tests have to
// answer is which *instant* the lookup was made at, and that needs a fake, not a database.
type InstrumentResolver interface {
	// Instrument returns the instrument behind an exchange symbol as it stood at `at`,
	// which is always the event's own event_time -- never time.Now() (L8).
	Instrument(ctx context.Context, market instrument.Market, symbol string, at time.Time) (int64, error)
}

// Resolver resolves both halves of what a fill names: the instrument behind its symbol and
// the asset behind its fee ticker. One interface because a fill needs both and splitting
// them would let a call site supply half of what an event costs.
type Resolver interface {
	InstrumentResolver
	AssetResolver
}

// IngestContext is everything the payload cannot say: whose event this is, and which path
// saw it. The exchange knows none of it, so it is passed in rather than guessed. Named for
// ingestion rather than trading because deposits travel the same way.
type IngestContext struct {
	AccountID     uuid.UUID
	IntegrationID uuid.UUID
	Source        string
}

// restTrade is one element of a myTrades response. Field names verified against the
// documented example on 2026-09-04; see testdata/fixtures/binance/mytrades_bnbbtc.json.
//
// Every money field is a string in the payload and stays a string here: decoding a
// quantity into float64 rewrites its digits, and every later number is checked against
// those digits (L1).
type restTrade struct {
	Symbol          string `json:"symbol"`
	ID              int64  `json:"id"`
	OrderID         int64  `json:"orderId"`
	Price           string `json:"price"`
	Qty             string `json:"qty"`
	QuoteQty        string `json:"quoteQty"`
	Commission      string `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`
	Time            int64  `json:"time"`
	IsBuyer         bool   `json:"isBuyer"`
	IsMaker         bool   `json:"isMaker"`
}

// executionReport is a spot user-data-stream order update, read key by key.
//
// It is deliberately NOT a struct with json tags. encoding/json matches object keys to
// fields case-insensitively when no exact match exists, and Binance's abbreviations differ
// only by case: `t` is the trade id and `T` the transaction time; `l` the last quantity and
// `L` the last price; `n` the commission and `N` its asset; `s` the symbol and `S` the side.
// One undeclared key is enough for a timestamp to land in a trade id, and the resulting
// event looks entirely plausible.
//
// Field meanings verified against binance-spot-api-docs on 2026-09-04.
type executionReport struct {
	ExecutionType string
	Symbol        string
	Side          string
	TradeID       int64
	LastQty       string
	LastPrice     string
	Commission    string
	CommissionAss string
	HasCommission bool
	TransactTime  int64
}

// rawObject is a JSON object whose keys are read exactly as written.
type rawObject map[string]json.RawMessage

// str reads a JSON string. A missing key and an explicit null are both the empty string
// with ok=false, which is what "N": null means -- no fee asset, rather than one named "".
func (o rawObject) str(key string) (string, bool, error) {
	raw, present := o[key]
	if !present || string(raw) == "null" {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, fmt.Errorf("field %q is not a string: %w", key, err)
	}
	return value, true, nil
}

// num reads a JSON number as an int64. Binance's ids exceed 2^53, so they are never routed
// through float64 (L1).
func (o rawObject) num(key string) (int64, error) {
	raw, present := o[key]
	if !present || string(raw) == "null" {
		return 0, nil
	}
	var value json.Number
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("field %q is not a number: %w", key, err)
	}
	return value.Int64()
}

// parseExecutionReport reads the payload by exact key. See the type's doc comment for why
// this cannot be a tagged struct.
func parseExecutionReport(raw json.RawMessage) (executionReport, error) {
	var object rawObject
	if err := json.Unmarshal(raw, &object); err != nil {
		return executionReport{}, fmt.Errorf("%w: decode executionReport: %w", ErrMalformedTrade, err)
	}

	var report executionReport
	var err error
	read := func(key string) string {
		if err != nil {
			return ""
		}
		var value string
		value, _, err = object.str(key)
		return value
	}
	readNum := func(key string) int64 {
		if err != nil {
			return 0
		}
		var value int64
		value, err = object.num(key)
		return value
	}

	report.ExecutionType = read("x")
	report.Symbol = read("s")
	report.Side = read("S")
	report.LastQty = read("l")
	report.LastPrice = read("L")
	report.Commission = read("n")
	report.TradeID = readNum("t")
	report.TransactTime = readNum("T")
	if err == nil {
		report.CommissionAss, report.HasCommission, err = object.str("N")
	}
	if err != nil {
		return executionReport{}, fmt.Errorf("%w: %w", ErrMalformedTrade, err)
	}
	return report, nil
}

// executionTypeTrade is the only execution type that moves a position.
const executionTypeTrade = "TRADE"

// nonFillExecutionTypes are the understood ways an order changes without a fill. Listed
// explicitly rather than treated as a default: the default has to be "we do not know what
// this is", so that a type Binance adds later stops the ingest instead of being dropped.
var nonFillExecutionTypes = map[string]bool{
	"NEW":              true,
	"CANCELED":         true,
	"REPLACED":         true,
	"REJECTED":         true,
	"EXPIRED":          true,
	"TRADE_PREVENTION": true,
}

// NormalizeSpotTrade turns one element of a myTrades response into a canonical event.
//
// raw is stored verbatim (L15): when a normalization bug surfaces months from now, the
// thing that saves the project is replaying these exact bytes.
func NormalizeSpotTrade(
	ctx context.Context, r Resolver, tc IngestContext, raw json.RawMessage,
) (ledger.Event, error) {
	var trade restTrade
	if err := json.Unmarshal(raw, &trade); err != nil {
		return ledger.Event{}, fmt.Errorf("%w: decode myTrades element: %w", ErrMalformedTrade, err)
	}
	side := ledger.SideSell
	if trade.IsBuyer {
		side = ledger.SideBuy
	}
	return buildTrade(ctx, r, tc, tradeFields{
		symbol:     trade.Symbol,
		tradeID:    trade.ID,
		side:       side,
		quantity:   trade.Qty,
		price:      trade.Price,
		fee:        trade.Commission,
		feeAsset:   trade.CommissionAsset,
		timeMillis: trade.Time,
		raw:        raw,
	})
}

// NormalizeStreamExecutionReport turns a user-data-stream order update into a canonical
// event. It returns ErrNotAFill for the many updates that are not trades; the caller skips
// those and treats every other error as a data-quality finding (K14).
func NormalizeStreamExecutionReport(
	ctx context.Context, r Resolver, tc IngestContext, raw json.RawMessage,
) (ledger.Event, error) {
	report, err := parseExecutionReport(raw)
	if err != nil {
		return ledger.Event{}, err
	}

	switch {
	case report.ExecutionType == executionTypeTrade:
	case nonFillExecutionTypes[report.ExecutionType]:
		return ledger.Event{}, fmt.Errorf("%w: x=%s", ErrNotAFill, report.ExecutionType)
	default:
		return ledger.Event{}, fmt.Errorf("%w: x=%q on %s, refusing to guess whether it moved a position",
			ErrUnknownExecutionType, report.ExecutionType, report.Symbol)
	}

	side, err := parseSide(report.Side)
	if err != nil {
		return ledger.Event{}, err
	}

	// N is null when nothing was charged. The schema requires fee and fee_asset to be null
	// together, so an unnamed fee becomes no fee at all rather than half a row.
	feeAsset := ""
	if report.HasCommission {
		feeAsset = report.CommissionAss
	}

	return buildTrade(ctx, r, tc, tradeFields{
		symbol:     report.Symbol,
		tradeID:    report.TradeID,
		side:       side,
		quantity:   report.LastQty,
		price:      report.LastPrice,
		fee:        report.Commission,
		feeAsset:   feeAsset,
		timeMillis: report.TransactTime,
		raw:        raw,
	})
}

// tradeFields is what the two payload shapes have in common once their spelling is
// removed. Both paths go through one builder on purpose: the identity and the numbers are
// then produced by the same code, which is what makes them equal by construction rather
// than by two implementations happening to agree (L5).
type tradeFields struct {
	symbol     string
	tradeID    int64
	side       ledger.Side
	quantity   string
	price      string
	fee        string
	feeAsset   string
	timeMillis int64
	raw        json.RawMessage
}

func buildTrade(
	ctx context.Context, r Resolver, tc IngestContext, f tradeFields,
) (ledger.Event, error) {
	if f.symbol == "" {
		return ledger.Event{}, fmt.Errorf("%w: no symbol", ErrMalformedTrade)
	}
	// A fill with no trade id has no identity, and an event with no identity cannot be
	// deduplicated. Binance writes -1 where there is no trade.
	if f.tradeID <= 0 {
		return ledger.Event{}, fmt.Errorf("%w: %s reports a fill with trade id %d",
			ErrMalformedTrade, f.symbol, f.tradeID)
	}
	if f.timeMillis <= 0 {
		return ledger.Event{}, fmt.Errorf("%w: %s trade %d has no exchange timestamp",
			ErrMalformedTrade, f.symbol, f.tradeID)
	}

	// The exchange's own time, and the instant the symbol is resolved at. Everything
	// downstream is calculated from it; our clock never enters (K2, L8).
	eventTime := time.UnixMilli(f.timeMillis).UTC()

	quantity, err := parseAmount(f.quantity)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("%w: %s trade %d quantity: %w",
			ErrMalformedTrade, f.symbol, f.tradeID, err)
	}
	if !quantity.IsPositive() {
		return ledger.Event{}, fmt.Errorf("%w: %s trade %d has quantity %s",
			ErrMalformedTrade, f.symbol, f.tradeID, quantity)
	}
	price, err := parseAmount(f.price)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("%w: %s trade %d price: %w",
			ErrMalformedTrade, f.symbol, f.tradeID, err)
	}
	if price.IsNegative() {
		return ledger.Event{}, fmt.Errorf("%w: %s trade %d has price %s",
			ErrMalformedTrade, f.symbol, f.tradeID, price)
	}

	instrumentID, err := r.Instrument(ctx, instrument.MarketSpot, f.symbol, eventTime)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("normalize %s trade %d: %w", f.symbol, f.tradeID, err)
	}

	fee, feeAsset, feeAssetID, err := resolveFee(ctx, r, f.fee, f.feeAsset, eventTime)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("normalize %s trade %d fee: %w",
			f.symbol, f.tradeID, err)
	}

	return ledger.Event{
		AccountID:     tc.AccountID,
		IntegrationID: tc.IntegrationID,

		// Built from exchange fields alone. The source is not in it, and that is the whole
		// point of the identity (K19, L5).
		VenueEventID:  SpotTradeID(f.symbol, f.tradeID),
		VenueSequence: f.tradeID,
		Source:        tc.Source,

		EventType:    ledger.TypeTrade,
		InstrumentID: &instrumentID,
		Side:         f.side,

		Quantity: decimal.NullDecimal{Decimal: quantity, Valid: true},
		Price:    decimal.NullDecimal{Decimal: price, Valid: true},

		// The fee rides on the event that caused it and is never folded into the price
		// (K18, L9). Folding it is invisible on one trade and wrong on every cost basis
		// after it.
		Fee:        fee,
		FeeAsset:   feeAsset,
		FeeAssetID: feeAssetID,

		EventTime: eventTime,
		Raw:       f.raw,
	}, nil
}

// SpotTradeID is the canonical identity of a spot fill. Exported because it is the single
// definition both ingest paths and every test must agree on; a second spelling of this
// string anywhere is a doubled position waiting to happen.
func SpotTradeID(symbol string, tradeID int64) string {
	return fmt.Sprintf("spot:trade:%s:%d", symbol, tradeID)
}

func parseSide(raw string) (ledger.Side, error) {
	switch raw {
	case "BUY":
		return ledger.SideBuy, nil
	case "SELL":
		return ledger.SideSell, nil
	default:
		// Never defaulted. A side we cannot read, silently treated as a buy, inverts the
		// position and every number after it stays plausible.
		return "", fmt.Errorf("%w: side %q", ErrMalformedTrade, raw)
	}
}

// parseAmount reads a money field. decimal.NewFromString only: a float64 on this path is a
// bug even when the test passes (L1).
func parseAmount(raw string) (decimal.Decimal, error) {
	if raw == "" {
		return decimal.Decimal{}, errors.New("empty")
	}
	return decimal.NewFromString(raw)
}

// parseFee returns no fee when nothing was charged. A zero fee in an unnamed asset is what
// Binance sends for a free fill, and the schema requires fee and fee_asset to be null
// together -- so it becomes two nulls rather than one of each.
func parseFee(raw, asset string) (decimal.NullDecimal, error) {
	if raw == "" {
		return decimal.NullDecimal{}, nil
	}
	amount, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.NullDecimal{}, err
	}
	if amount.IsZero() && asset == "" {
		return decimal.NullDecimal{}, nil
	}
	if asset == "" {
		return decimal.NullDecimal{}, fmt.Errorf("fee %s has no asset", amount)
	}
	return decimal.NullDecimal{Decimal: amount, Valid: true}, nil
}

// resolveFee parses a fee and resolves the asset it was paid in, as of the event's own
// event_time (L8, K22). Shared by every normalizer, because "what happens when a fee's
// ticker is not in the registry" is one rule (K45) and a second implementation of it is a
// second answer.
//
// Resolved here rather than at fold time, because here is where the event's own time is
// unambiguously in hand. Resolving it later means resolving it with whatever mapping is
// current then, which is the industry's number-one silent corruption.
//
// An unknown ticker is swallowed on purpose, and only that one: a coin we have not curated
// yet must not cost us the fill, which is the irreplaceable half. The fee's balance effect is
// then refused rather than guessed, and the reader is told (unknown_symbol, L11). Any other
// error -- the database being unreachable, say -- still fails the normalization, because
// silently dropping every fee for the duration of an outage is not the same thing at all.
func resolveFee(
	ctx context.Context, r AssetResolver, rawFee, rawAsset string, eventTime time.Time,
) (decimal.NullDecimal, string, *int64, error) {
	fee, err := parseFee(rawFee, rawAsset)
	if err != nil {
		return decimal.NullDecimal{}, "", nil, fmt.Errorf("%w: %w", ErrMalformedTrade, err)
	}

	feeAsset := rawAsset
	if !fee.Valid {
		// fee and fee_asset are null together or not at all.
		feeAsset = ""
	}

	var feeAssetID *int64
	if feeAsset != "" {
		id, err := r.Asset(ctx, feeAsset, eventTime)
		switch {
		case err == nil:
			feeAssetID = &id
		case errors.Is(err, asset.ErrUnknownSymbol):
		default:
			return decimal.NullDecimal{}, "", nil, fmt.Errorf("asset %q: %w", feeAsset, err)
		}
	}
	return fee, feeAsset, feeAssetID, nil
}
