package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/ledger"
	"github.com/shopspring/decimal"
)

// ErrUnknownTransferType means the venue reported a transfer direction this milestone does
// not walk. Loud rather than skipped: 32 types are documented and eight are walked, so an
// unrecognized one is either a wallet the schema has no word for or a type Binance added
// after this table was written. Resolving it to a plausible wallet would move money between
// two wallets that are not the ones it moved between, and nothing downstream could tell.
var ErrUnknownTransferType = errors.New("binance: unknown transfer type")

// ErrTransferNotConfirmed means the row is not in the one status this normalizer accepts.
//
// It is a whitelist because the status enum is not published (F11): the page shows `status`
// as a string and gives exactly one value, CONFIRMED, in its response example, enumerating
// nothing else. A blacklist would need the list of bad values, which does not exist -- so
// the safe direction is to record the one meaning that is documented and refuse the rest.
// Recording a transfer that has not happened misstates which wallet holds the money, which
// is precisely the question M5 asks.
var ErrTransferNotConfirmed = errors.New("binance: transfer is not confirmed")

// statusConfirmed is the only value the documentation shows. Compared exactly, not
// case-insensitively: a venue that changes the casing has changed the field, and finding
// that out loudly is better than accommodating it silently.
const statusConfirmed = "CONFIRMED"

// The wallet vocabulary, matching the schema's (00020). `external` is not here because no
// intra-venue transfer has an outside: it is M8's, for the far side of a cross-venue move.
const (
	walletSpot    = "spot"
	walletUSDM    = "usdm"
	walletCoinM   = "coinm"
	walletMargin  = "margin"
	walletFunding = "funding"
)

// walkedTransfers maps the eight directions this milestone walks onto the two wallets each
// one runs between.
//
// A table rather than a parser, though the venue's names look parseable: MAIN_UMFUTURE
// splits neatly on the underscore, and ISOLATEDMARGIN_MARGIN does not mean what splitting
// it would suggest. A parser would answer for every one of the 32 documented types,
// including the ones naming wallets this schema has no word for -- confidently, and
// wrongly. The table answers only for what has been verified, and ErrUnknownTransferType
// covers the rest.
//
// The eight are the ones a one-way-mode spot and futures account actually uses; the other
// 24 involve isolated margin, sub-accounts and options, none of which V1 models.
var walkedTransfers = map[string][2]string{
	"MAIN_UMFUTURE": {walletSpot, walletUSDM},
	"UMFUTURE_MAIN": {walletUSDM, walletSpot},
	"MAIN_CMFUTURE": {walletSpot, walletCoinM},
	"CMFUTURE_MAIN": {walletCoinM, walletSpot},
	"MAIN_MARGIN":   {walletSpot, walletMargin},
	"MARGIN_MAIN":   {walletMargin, walletSpot},
	"MAIN_FUNDING":  {walletSpot, walletFunding},
	"FUNDING_MAIN":  {walletFunding, walletSpot},
}

// WalletsOf resolves a venue transfer type to the two wallets it runs between, in order.
//
// Exported because the backfill needs the same knowledge to know which eight queries to
// make: `type` is a required parameter on the endpoint, so "every transfer" is one call per
// direction (F10). One table, read by both, is what stops the walk and the normalizer from
// disagreeing about which directions exist.
func WalletsOf(transferType string) (from, to string, err error) {
	wallets, ok := walkedTransfers[transferType]
	if !ok {
		return "", "", fmt.Errorf("%w: %q", ErrUnknownTransferType, transferType)
	}
	return wallets[0], wallets[1], nil
}

// WalkedTransferTypes returns the eight directions, sorted so a walk over them is
// deterministic.
func WalkedTransferTypes() []string {
	out := make([]string, 0, len(walkedTransfers))
	for t := range walkedTransfers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// transferRow is one element of a universal-transfer-history response. Field names verified
// against the documented example on 2026-09-09; see
// testdata/fixtures/binance/universal_transfer.json.
//
// The amount stays a string all the way to decimal: decoding it into float64 rewrites its
// digits, and every later number is checked against those digits (L1).
type transferRow struct {
	Asset  string `json:"asset"`
	Amount string `json:"amount"`

	// Type is the direction, not a category. It is also half of the identity (F13).
	Type   string `json:"type"`
	Status string `json:"status"`

	// TranID is an int64 per row. F3 found that on futures income it is unique per
	// incomeType rather than globally, and nothing on this page claims anything stronger
	// for transfers -- which is why the identity carries the type as well.
	TranID    int64 `json:"tranId"`
	Timestamp int64 `json:"timestamp"`
}

// NormalizeTransfer turns one universal-transfer-history row into a canonical event.
//
// One row is the whole movement: the venue reports an intra-venue transfer with the
// direction inside the required `type` parameter, so there are no two halves to match (F10).
// The quantity is therefore unsigned -- the endpoints carry the direction, and a magnitude
// that was also a direction would be two facts in one column, free to disagree.
//
// raw is stored verbatim (L15).
func NormalizeTransfer(
	ctx context.Context, r AssetResolver, ic IngestContext, raw json.RawMessage,
) (ledger.Event, error) {
	var row transferRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return ledger.Event{}, fmt.Errorf("%w: decode transfer: %w", ErrMalformedTrade, err)
	}

	if row.Status != statusConfirmed {
		return ledger.Event{}, fmt.Errorf(
			"%w: status %q on transfer %d, and only %q is documented",
			ErrTransferNotConfirmed, row.Status, row.TranID, statusConfirmed)
	}

	from, to, err := WalletsOf(row.Type)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("normalize transfer %d: %w", row.TranID, err)
	}

	if row.TranID == 0 {
		return ledger.Event{}, fmt.Errorf(
			"%w: transfer has no transaction id, so it has no identity", ErrMalformedTrade)
	}
	if row.Asset == "" {
		return ledger.Event{}, fmt.Errorf("%w: transfer %d names no asset",
			ErrMalformedTrade, row.TranID)
	}
	if row.Timestamp <= 0 {
		return ledger.Event{}, fmt.Errorf("%w: transfer %d has no exchange timestamp",
			ErrMalformedTrade, row.TranID)
	}

	amount, err := parseAmount(row.Amount)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("%w: transfer %d amount: %w",
			ErrMalformedTrade, row.TranID, err)
	}
	if !amount.IsPositive() {
		// A sign here would be a direction travelling in the column the endpoints already
		// own, and the balance engine refuses it for the same reason.
		return ledger.Event{}, fmt.Errorf(
			"%w: transfer %d has amount %s; the direction is in its type",
			ErrMalformedTrade, row.TranID, amount)
	}

	eventTime := time.UnixMilli(row.Timestamp).UTC()
	assetID, err := r.Asset(ctx, row.Asset, eventTime)
	if err != nil {
		return ledger.Event{}, fmt.Errorf("normalize transfer %d (%s): %w",
			row.TranID, row.Asset, err)
	}

	return ledger.Event{
		AccountID:     ic.AccountID,
		IntegrationID: ic.IntegrationID,

		VenueEventID: TransferID(row.Type, row.TranID),
		// tranId is not documented as monotonic across the account, so it is not used as
		// the ordering tiebreak. Zero sorts first within its timestamp, and venue_event_id
		// -- the third level of the canonical order -- still makes the order total (L7).
		VenueSequence: 0,
		Source:        ic.Source,

		EventType: ledger.TypeTransfer,
		// No instrument and no price. A transfer moves one asset between two wallets;
		// giving it a price would hand it a cost basis it never had (K18).
		AssetID:      &assetID,
		TransferFrom: from,
		TransferTo:   to,
		Quantity:     decimal.NullDecimal{Decimal: amount, Valid: true},

		EventTime: eventTime,
		Raw:       raw,
	}, nil
}

// TransferID is the canonical identity of a transfer: the type, then the venue's id (F13).
// Exported for the same reason SpotTradeID is -- a second spelling anywhere is a doubled
// balance waiting to happen (L5).
func TransferID(transferType string, tranID int64) string {
	return fmt.Sprintf("transfer:%s:%d", transferType, tranID)
}
