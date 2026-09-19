package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/instrument"
	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"
)

// ErrUnknownIncomeType means the venue reported a cash flow this fold has no rule for.
//
// The enum cannot be read off the page in full: it lists eight values and refers to fifteen
// more it does not display (F16). So this is a whitelist, and an unrecognized type stops the
// walk rather than passing through as zero -- an unknown cash flow silently folded as
// nothing is a balance that drifts from the exchange's by exactly the amount nobody looked
// at, which is the failure the negative-balance check exists to catch and would not.
var ErrUnknownIncomeType = errors.New("binance: unknown income type")

// ErrIncomeReportedElsewhere means the row is a second copy of something already ingested,
// and folding it would count that thing twice.
//
// A skip rather than a failure, and a NAMED skip rather than a silent one: "we chose not to
// fold this" and "we forgot to fold this" must not look the same in a log six months from
// now, when someone is asking why the futures wallet is short.
var ErrIncomeReportedElsewhere = errors.New("binance: income row duplicates an event ingested elsewhere")

// The income types this fold has a rule for. Every other value is ErrUnknownIncomeType.
const (
	incomeFunding     = "FUNDING_FEE"
	incomeTransfer    = "TRANSFER"
	incomeRealizedPnl = "REALIZED_PNL"
	incomeCommission  = "COMMISSION"
)

// incomeRow is one element of an income-history response (F16). `income` is signed and
// stays a string until decimal (L1).
type incomeRow struct {
	Symbol     string `json:"symbol"`
	IncomeType string `json:"incomeType"`
	Income     string `json:"income"`
	Asset      string `json:"asset"`
	Time       int64  `json:"time"`

	// TranID is unique within an incomeType rather than globally -- quoted from the page,
	// typo included: "trandId is unique in the same incomeType for a user" (F3, F16).
	TranID int64 `json:"tranId"`
}

// NormalizeIncome turns one USD-M income row into a canonical event, or reports why it was
// deliberately not turned into one.
//
// raw is stored verbatim (L15).
func NormalizeIncome(
	ctx context.Context, r Resolver, ic IngestContext, raw json.RawMessage,
) (ledger.Event, error) {
	var row incomeRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return ledger.Event{}, fmt.Errorf("%w: decode income element: %w", ErrMalformedTrade, err)
	}

	switch row.IncomeType {
	case incomeFunding:
		// The one type this fold owns.

	case incomeTransfer:
		// F12: the same movement the wallet endpoint reported, which M3.5 already
		// ingests. Because an internal transfer folds to no delta, a second copy here
		// would not even surface as a doubled balance -- it would surface as a withdrawal
		// from the futures wallet that never happened.
		return ledger.Event{}, fmt.Errorf(
			"%w: transfer %d is the wallet endpoint's row seen again (F12)",
			ErrIncomeReportedElsewhere, row.TranID)

	case incomeRealizedPnl:
		// The venue's copy of a number the position engine computes (K5, L3).
		return ledger.Event{}, fmt.Errorf(
			"%w: realized pnl %d is computed from the fold, never ingested (K5)",
			ErrIncomeReportedElsewhere, row.TranID)

	case incomeCommission:
		// The fee already rides on the fill that caused it (L9, K18). Folding it again is
		// the quietest of the three duplicates: a doubled fee reads as a slightly worse
		// fill rather than as a bug.
		return ledger.Event{}, fmt.Errorf(
			"%w: commission %d already rides on its fill (L9)",
			ErrIncomeReportedElsewhere, row.TranID)

	default:
		return ledger.Event{}, fmt.Errorf("%w: %q on income %d, refusing to fold it as zero",
			ErrUnknownIncomeType, row.IncomeType, row.TranID)
	}

	if row.TranID == 0 {
		return ledger.Event{}, fmt.Errorf(
			"%w: income row has no transaction id, so it has no identity", ErrMalformedTrade)
	}
	if row.Symbol == "" {
		// FUNDING_PAYMENT's amount is in the settle asset OF AN INSTRUMENT; with no symbol
		// there is no position for it to belong to and nowhere to put it.
		return ledger.Event{}, fmt.Errorf("%w: funding payment %d names no symbol",
			ErrMalformedTrade, row.TranID)
	}
	if row.Time <= 0 {
		return ledger.Event{}, fmt.Errorf("%w: income %d has no exchange timestamp",
			ErrMalformedTrade, row.TranID)
	}

	// Signed, and kept signed. Funding paid is negative and funding received is positive;
	// one event type with a sign beats two types that would have to agree with each other
	// forever, and taking the absolute value here would turn every payment into a cost.
	amount, err := parseAmount(row.Income)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("%w: income %d amount: %w",
			ErrMalformedTrade, row.TranID, err)
	}

	eventTime := time.UnixMilli(row.Time).UTC()
	instrumentID, err := r.Instrument(ctx, instrument.MarketUSDM, row.Symbol, eventTime)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("normalize funding %d (%s): %w",
			row.TranID, row.Symbol, err)
	}

	return ledger.Event{
		AccountID:     ic.AccountID,
		IntegrationID: ic.IntegrationID,

		VenueEventID: IncomeID(row.IncomeType, row.TranID),
		// tranId is unique only within its incomeType, so it is not a monotonic sequence
		// across the account and is not used as the ordering tiebreak. Zero sorts first
		// within its timestamp and venue_event_id makes the order total (L7).
		VenueSequence: 0,
		Source:        ic.Source,

		EventType:    ledger.TypeFundingPayment,
		InstrumentID: &instrumentID,

		// No price and no side: funding is a cash flow, not a fill. A price would hand it
		// a cost basis and a side would make it look like one (K18).
		Quantity: decimal.NullDecimal{Decimal: amount, Valid: true},

		EventTime: eventTime,
		Raw:       raw,
	}, nil
}

// IncomeID is the canonical identity of an income row. The type is in it because the venue
// only promises uniqueness within a type (F3, F16): an identity built from tranId alone
// would let a funding payment and a commission with the same number collapse into one
// event, and the survivor would be whichever was walked first.
func IncomeID(incomeType string, tranID int64) string {
	return fmt.Sprintf("usdm:income:%s:%d", incomeType, tranID)
}
