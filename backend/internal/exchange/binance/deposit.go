package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"
)

// ErrNotSettled means the row describes money that is not in the account: a deposit still
// confirming, rejected, or waiting on the user. It is a skip rather than an error, and it
// is deliberately not silence -- crediting a pending deposit would put a balance in the
// ledger that may never arrive, and the ledger is append-only, so undoing it means a second
// event reversing the first (L2).
var ErrNotSettled = errors.New("binance: deposit has not settled")

// ErrUnknownDepositStatus means Binance reported a status code the documentation does not
// list. Loud on purpose, in both directions: guessing "settled" credits money that was
// never received, and guessing "not settled" loses a deposit and leaves a hole the cost
// basis never recovers from (K26).
var ErrUnknownDepositStatus = errors.New("binance: unknown deposit status")

// AssetResolver resolves a coin ticker to a canonical asset as of a point in time. Separate
// from the instrument resolver because a deposit moves one asset while a trade moves a
// pair, and a function should not have to accept a dependency it never uses.
type AssetResolver interface {
	// Asset resolves as of the event's own event_time, never time.Now(): exchanges recycle
	// tickers after a delisting, and today's mapping attaches a correct quantity to the
	// wrong asset (K22, L8).
	Asset(ctx context.Context, symbol string, at time.Time) (int64, error)
}

// depositStatus records what each documented code means for the ledger. Quoted from the
// deposit-history page on 2026-09-04: "0: pending, 1: success, 2: rejected, 6: credited but
// cannot withdraw, 7: Wrong Deposit, 8: Waiting User confirm".
//
// A map rather than a range check, so a code Binance adds later is absent rather than
// silently classified.
var depositSettled = map[int]bool{
	0: false, // pending -- confirming on chain, not in the account yet
	1: true,  // success
	2: false, // rejected
	6: true,  // credited but cannot withdraw: the coins ARE in the account and count toward
	//          the portfolio. Only moving them out is blocked, which the ledger does not model.
	7: false, // wrong deposit
	8: false, // waiting user confirm
}

// depositRow is one element of a deposit-history response. Amounts stay strings: a quantity
// decoded into float64 comes back with different digits (L1).
//
// Field names verified against the deposit-history page on 2026-09-04; see
// testdata/fixtures/binance/deposits.json.
type depositRow struct {
	// ID is the venue's own record id, and the event's identity. Deliberately not TxId:
	// a chain hash links the account to a wallet (so fixtures redact it), an internal
	// transfer has no txId at all, and nothing makes one hash mean exactly one row.
	ID         string `json:"id"`
	Amount     string `json:"amount"`
	Coin       string `json:"coin"`
	Status     int    `json:"status"`
	InsertTime int64  `json:"insertTime"`
}

// NormalizeDeposit turns one deposit-history row into a canonical event, or returns
// ErrNotSettled for a row that does not describe money in the account.
//
// raw is stored verbatim (L15).
func NormalizeDeposit(
	ctx context.Context, r AssetResolver, ic IngestContext, raw json.RawMessage,
) (ledger.Event, error) {
	var row depositRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return ledger.Event{}, fmt.Errorf("%w: decode deposit: %w", ErrMalformedTrade, err)
	}

	settled, known := depositSettled[row.Status]
	switch {
	case !known:
		return ledger.Event{}, fmt.Errorf(
			"%w: status %d on deposit %s, refusing to guess whether the money arrived",
			ErrUnknownDepositStatus, row.Status, row.ID)
	case !settled:
		return ledger.Event{}, fmt.Errorf("%w: status %d", ErrNotSettled, row.Status)
	}

	if row.ID == "" {
		return ledger.Event{}, fmt.Errorf("%w: deposit has no record id, so it has no identity",
			ErrMalformedTrade)
	}
	if row.Coin == "" {
		return ledger.Event{}, fmt.Errorf("%w: deposit %s names no coin", ErrMalformedTrade, row.ID)
	}
	if row.InsertTime <= 0 {
		return ledger.Event{}, fmt.Errorf("%w: deposit %s has no exchange timestamp",
			ErrMalformedTrade, row.ID)
	}

	amount, err := parseAmount(row.Amount)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("%w: deposit %s amount: %w", ErrMalformedTrade, row.ID, err)
	}
	if !amount.IsPositive() {
		return ledger.Event{}, fmt.Errorf("%w: deposit %s has amount %s",
			ErrMalformedTrade, row.ID, amount)
	}

	eventTime := time.UnixMilli(row.InsertTime).UTC()
	assetID, err := r.Asset(ctx, row.Coin, eventTime)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("normalize deposit %s (%s): %w", row.ID, row.Coin, err)
	}

	return ledger.Event{
		AccountID:     ic.AccountID,
		IntegrationID: ic.IntegrationID,

		VenueEventID: SpotDepositID(row.ID),
		// The venue's id is a string and is not documented as monotonic, so it is not used
		// as the ordering tiebreak. Zero sorts first within its timestamp, and the third
		// level of the canonical order -- venue_event_id -- still makes the order total
		// and deterministic (L7).
		VenueSequence: 0,
		Source:        ic.Source,

		EventType: ledger.TypeDeposit,
		// No instrument and no price. A deposit moves one asset; giving it a price of zero
		// would hand it a cost basis it never had (K18).
		AssetID:  &assetID,
		Quantity: decimal.NullDecimal{Decimal: amount, Valid: true},

		EventTime: eventTime,
		Raw:       raw,
	}, nil
}

// SpotDepositID is the canonical identity of a deposit. Exported for the same reason
// SpotTradeID is: a second spelling anywhere is a doubled balance waiting to happen.
func SpotDepositID(recordID string) string {
	return fmt.Sprintf("spot:deposit:%s", recordID)
}
