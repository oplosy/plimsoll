package bybit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	// ErrUnknownStatus means the venue reported a status this code has never heard of.
	//
	// Refused rather than mapped to the nearest plausible meaning: which status means "the
	// coins moved" decides whether a balance is right, and a new code guessed at is a wrong
	// balance that looks exactly like a right one.
	ErrUnknownStatus = errors.New("bybit: unknown status, refusing to guess whether the money moved")

	// ErrNotSettled means the movement has not completed. Not an error in the record -- an
	// answer about a movement that has not happened yet.
	ErrNotSettled = errors.New("bybit: not settled")

	// ErrMalformed means the row cannot be read as a movement at all.
	ErrMalformed = errors.New("bybit: malformed record")
)

// depositSettled is the DepositStatus enum, quoted from the venue's enum page (B2).
//
// A closed map, and the value is "did the coins arrive". 70012 is the subtle one: a deposit
// that REMAINS successful after a rollback review is settled, while 70011 -- rolled back -- is
// not, and the two differ by one digit.
var depositSettled = map[int]bool{
	0:     false, // unknown
	1:     false, // toBeConfirmed
	2:     false, // processing
	3:     true,  // success
	4:     false, // deposit failed
	7:     false, // Rollback processing
	70011: false, // Deposit rolled back
	70012: true,  // Deposit remains successful after rollback review
	70013: false, // Deposit rollback fail
	10011: false, // pending to be credited to funding pool
	10012: true,  // Credited to funding pool successfully
}

// withdrawSettled is the WithdrawStatus enum, quoted from the same page (B2). A string enum,
// unlike the deposit one.
//
// Only `success` means the coins left. `BlockchainConfirmed` is deliberately NOT settled: the
// chain has confirmed the transaction, which is not the same claim as the venue having
// finished the withdrawal, and the enum lists both -- so treating them as one would be reading
// a distinction the venue drew and deciding it did not mean it.
var withdrawSettled = map[string]bool{
	"SecurityCheck":                   false,
	"Pending":                         false,
	"success":                         true,
	"CancelByUser":                    false,
	"Reject":                          false,
	"Fail":                            false,
	"BlockchainConfirmed":             false,
	"MoreInformationRequired":         false,
	"Unknown":                         false,
	"HighValueReviewPending":          false,
	"HighValueReviewEDDSubmission":    false,
	"HighValueReviewRejected":         false,
	"HighValueReviewRejectedRfunding": false,
	"HighValueReviewRejectedRefunded": false,
}

// AssetResolver maps a venue's coin code to a canonical asset, as of an instant (L8, K22).
type AssetResolver interface {
	Asset(ctx context.Context, symbol string, at time.Time) (int64, error)
}

// IngestContext is who this event belongs to.
type IngestContext struct {
	AccountID     uuid.UUID
	IntegrationID uuid.UUID
	Source        string
}

// DepositID is a deposit's canonical identity. Exported for the same reason Binance's is: a
// second spelling of an identity anywhere is a doubled balance waiting to happen (L5).
func DepositID(recordID string) string { return "bybit:deposit:" + recordID }

// WithdrawalID is a withdrawal's canonical identity, on the same rule.
func WithdrawalID(recordID string) string { return "bybit:withdrawal:" + recordID }

// NormalizeDeposit turns one deposit row into a ledger event.
func NormalizeDeposit(
	ctx context.Context, r AssetResolver, ic IngestContext, raw json.RawMessage,
) (ledger.Event, error) {
	var row struct {
		ID        string `json:"id"`
		Coin      string `json:"coin"`
		Amount    string `json:"amount"`
		Status    *int   `json:"status"`
		SuccessAt string `json:"successAt"`
		TxID      string `json:"txID"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return ledger.Event{}, fmt.Errorf("%w: decode deposit: %w", ErrMalformed, err)
	}
	if row.Status == nil {
		return ledger.Event{}, fmt.Errorf("%w: deposit %s reports no status", ErrMalformed, row.ID)
	}
	settled, known := depositSettled[*row.Status]
	switch {
	case !known:
		return ledger.Event{}, fmt.Errorf("%w: deposit status %d", ErrUnknownStatus, *row.Status)
	case !settled:
		return ledger.Event{}, fmt.Errorf("%w: deposit status %d", ErrNotSettled, *row.Status)
	}

	at, err := epochMillis(row.SuccessAt)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("%w: deposit %s: %w", ErrMalformed, row.ID, err)
	}
	return balanceEvent(ctx, r, ic, raw, balanceRow{
		venueEventID: DepositID(row.ID),
		recordID:     row.ID,
		coin:         row.Coin,
		amount:       row.Amount,
		at:           at,
		eventType:    ledger.TypeDeposit,
	})
}

// NormalizeWithdrawal turns one withdrawal row into a ledger event.
//
// It exists, and its Binance counterpart does not. That is not an inconsistency to be tidied
// away: Binance publishes only a garbled fragment where its status enum should be, and
// encoding a remembered enum into append-only financial rows is what CLAUDE.md forbids
// (F5, B2).
func NormalizeWithdrawal(
	ctx context.Context, r AssetResolver, ic IngestContext, raw json.RawMessage,
) (ledger.Event, error) {
	var row struct {
		WithdrawID string `json:"withdrawId"`
		Coin       string `json:"coin"`
		Amount     string `json:"amount"`
		Status     string `json:"status"`
		UpdateTime string `json:"updateTime"`
		TxID       string `json:"txID"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return ledger.Event{}, fmt.Errorf("%w: decode withdrawal: %w", ErrMalformed, err)
	}
	settled, known := withdrawSettled[row.Status]
	switch {
	case !known:
		return ledger.Event{}, fmt.Errorf("%w: withdrawal status %q", ErrUnknownStatus, row.Status)
	case !settled:
		return ledger.Event{}, fmt.Errorf("%w: withdrawal status %q", ErrNotSettled, row.Status)
	}

	at, err := epochMillis(row.UpdateTime)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("%w: withdrawal %s: %w", ErrMalformed, row.WithdrawID, err)
	}
	return balanceEvent(ctx, r, ic, raw, balanceRow{
		venueEventID: WithdrawalID(row.WithdrawID),
		recordID:     row.WithdrawID,
		coin:         row.Coin,
		amount:       row.Amount,
		at:           at,
		eventType:    ledger.TypeWithdrawal,
	})
}

// balanceRow is the part of a deposit and a withdrawal that is the same, which is everything
// except which enum decided it settled.
type balanceRow struct {
	venueEventID string
	recordID     string
	coin         string
	amount       string
	at           time.Time
	eventType    ledger.EventType
}

// balanceEvent builds the event both normalizers produce.
//
// The quantity is POSITIVE for both directions. K49 retired the signed-quantity convention:
// with the direction carried by the event type, a sign is a second statement of the same fact,
// free to disagree with the first -- and a withdrawal of -500 would read as money arriving.
func balanceEvent(
	ctx context.Context, r AssetResolver, ic IngestContext,
	raw json.RawMessage, row balanceRow,
) (ledger.Event, error) {
	if row.recordID == "" {
		return ledger.Event{}, fmt.Errorf(
			"%w: record has no id, so it has no identity (L5)", ErrMalformed)
	}
	if row.coin == "" {
		return ledger.Event{}, fmt.Errorf("%w: record %s names no coin", ErrMalformed, row.recordID)
	}

	amount, err := decimal.NewFromString(row.amount)
	if err != nil {
		// The value is not echoed. A payload field is not a place we have proven a credential
		// cannot be (L13).
		return ledger.Event{}, fmt.Errorf(
			"%w: record %s has an amount that is not a number", ErrMalformed, row.recordID)
	}
	if !amount.IsPositive() {
		return ledger.Event{}, fmt.Errorf(
			"%w: record %s has amount %s; direction lives on the event type, never on the sign (K49)",
			ErrMalformed, row.recordID, amount)
	}

	assetID, err := r.Asset(ctx, row.coin, row.at)
	if err != nil {
		return ledger.Event{}, fmt.Errorf(
			"normalize %s %s (%s): %w", row.eventType, row.recordID, row.coin, err)
	}

	return ledger.Event{
		AccountID:     ic.AccountID,
		IntegrationID: ic.IntegrationID,

		VenueEventID: row.venueEventID,
		// The venue's id is a string and is not documented as monotonic, so it is not the
		// ordering tiebreak. Zero sorts first within its timestamp, and venue_event_id --
		// the third level of the canonical order -- still makes the order total (L7).
		VenueSequence: 0,
		Source:        ic.Source,

		EventType: row.eventType,
		// No instrument and no price: a deposit moves one asset, and giving it a price of
		// zero would hand it a cost basis it never had (K18).
		AssetID:  &assetID,
		Quantity: decimal.NullDecimal{Decimal: amount, Valid: true},

		EventTime: row.at,
		Raw:       raw,
	}, nil
}

// epochMillis reads one of Bybit's timestamps.
//
// They arrive as epoch milliseconds INSIDE A STRING -- `"1742738305000"` -- which is the one
// place this venue is easier than Binance, whose withdrawal times are `"2019-10-12 11:12:02"`
// with no timezone stated and are therefore unusable (B4, F5).
func epochMillis(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("no timestamp")
	}
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not epoch milliseconds", value)
	}
	if ms <= 0 {
		return time.Time{}, fmt.Errorf("timestamp %q is not a real instant", value)
	}
	return time.UnixMilli(ms).UTC(), nil
}
