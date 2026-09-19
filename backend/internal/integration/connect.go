package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	// ErrOverPermissioned means the key can do something other than read. K9 rejects it
	// rather than accepting it with a warning: a warning is a thing that gets dismissed,
	// and the permission it warned about outlives the dismissal.
	ErrOverPermissioned = errors.New("integration: api key has permissions beyond reading")

	// ErrNotReadable means the key cannot read. It is a separate error because it needs
	// the opposite fix -- add a permission, not remove one -- and telling a user to remove
	// something when they need to add something costs them a second failed attempt.
	ErrNotReadable = errors.New("integration: api key cannot read account data")

	// ErrMalformedRestrictions means the permission response was not the object we expect.
	// An HTML error page and a JSON array both land here rather than decoding into an
	// all-false struct that would look like a harmless key.
	ErrMalformedRestrictions = errors.New("integration: api key permission response is malformed")
)

// Permissions is the decoded GET /sapi/v1/account/apiRestrictions response. Every boolean
// the documentation lists has a field here, including the ones M2 does not use: a struct
// that silently drops fields is a struct that stops noticing when one of them turns on.
//
// Field list verified 2026-09-03 against
// https://developers.binance.com/docs/wallet/account/api-key-permission and recorded in
// docs/BINANCE-API-NOTES.md §4.
type Permissions struct {
	// IPRestricted is ipRestrict: a restriction, not a capability. True is the safer key,
	// so it never causes a rejection.
	IPRestricted bool `json:"ipRestrict"`

	// The two permissions a read-only key is allowed to have.
	Reading     bool `json:"enableReading"`
	FixReadOnly bool `json:"enableFixReadOnly"`

	// Everything below disqualifies the key. The documentation states no semantics for any
	// of these, so "permission" is read broadly -- see the note on capabilityAllowlist.
	Withdrawals            bool `json:"enableWithdrawals"`
	InternalTransfer       bool `json:"enableInternalTransfer"`
	Margin                 bool `json:"enableMargin"`
	Futures                bool `json:"enableFutures"`
	UniversalTransfer      bool `json:"permitsUniversalTransfer"`
	VanillaOptions         bool `json:"enableVanillaOptions"`
	FixAPITrade            bool `json:"enableFixApiTrade"`
	SpotAndMarginTrading   bool `json:"enableSpotAndMarginTrading"`
	PortfolioMarginTrading bool `json:"enablePortfolioMarginTrading"`
}

// capabilityAllowlist is the set of boolean fields allowed to be true. It is an allowlist
// rather than a denylist of known-bad permissions, and that is the whole design: Binance
// adds permissions, and a denylist would accept the next one by default. A key with a
// permission we have never heard of is rejected until someone decides otherwise.
var capabilityAllowlist = map[string]bool{
	"enableReading":     true,
	"enableFixReadOnly": true,
}

// nonCapabilityFields are booleans that carry no capability. ipRestrict is the one that
// matters: it is a restriction, so treating it as a permission would reject exactly the
// keys that are safest.
var nonCapabilityFields = map[string]bool{
	"ipRestrict": true,
}

// PermissionReader is the slice of an exchange client this package needs. Declaring it
// here rather than importing the client keeps the dependency pointing one way: the
// exchange adapter knows about credentials, and this package does not know about Binance.
type PermissionReader interface {
	// APIRestrictions returns the raw permission payload for cred. Raw, because the
	// unknown-field check below has to see fields no Go struct declares.
	APIRestrictions(ctx context.Context, cred Credential) (json.RawMessage, error)
}

// Verify reads the key's permissions and decides whether it may be used at all. It is the
// gate K9 describes: a key that can trade or withdraw never reaches the rest of the
// system, so no later code has to be careful with it.
//
// Errors never carry the credential (L13) -- the caller already knows which key it handed
// over, and the message is written to be safe to show a user.
func Verify(ctx context.Context, r PermissionReader, cred Credential) (Permissions, error) {
	raw, err := r.APIRestrictions(ctx, cred)
	if err != nil {
		return Permissions{}, fmt.Errorf("integration: read api key permissions: %w", err)
	}
	return ParsePermissions(raw)
}

// ParsePermissions decodes the payload and applies the policy. It is exported separately
// from Verify so the policy can be tested against fixtures without a client, which is
// where every case that matters lives.
func ParsePermissions(raw json.RawMessage) (Permissions, error) {
	// Decoded twice on purpose. The struct gives named fields to work with; the map is the
	// only way to see a permission Binance shipped after this code was written.
	var perms Permissions
	if err := json.Unmarshal(raw, &perms); err != nil {
		return Permissions{}, fmt.Errorf("%w: %w", ErrMalformedRestrictions, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Permissions{}, fmt.Errorf("%w: %w", ErrMalformedRestrictions, err)
	}
	if fields == nil {
		// JSON null decodes into a nil map without error, and would otherwise pass through
		// as a key with every permission off.
		return Permissions{}, fmt.Errorf("%w: response is null", ErrMalformedRestrictions)
	}

	if offending := disqualifying(fields); len(offending) > 0 {
		return Permissions{}, fmt.Errorf(
			"%w: turn off %s on this key, or issue a separate read-only key",
			ErrOverPermissioned, strings.Join(offending, ", "))
	}
	if !perms.Reading {
		return Permissions{}, fmt.Errorf(
			"%w: enableReading is off, so the key cannot fetch balances or trades",
			ErrNotReadable)
	}
	return perms, nil
}

// disqualifying returns every field name that grants something beyond reading, sorted so
// the message is stable. It returns all of them rather than the first: a user who removes
// one permission and reconnects should not discover the next one on the next attempt.
func disqualifying(fields map[string]any) []string {
	var offending []string
	for name, value := range fields {
		// Keys we added to the fixture files. They are strings, so the type check below
		// already skips them; skipping by prefix keeps that true if one is ever a bool.
		if strings.HasPrefix(name, "_") {
			continue
		}
		enabled, isBool := value.(bool)
		if !isBool || !enabled {
			// Not a permission, or not granted. A timestamp Binance adds next year lands
			// here rather than blocking every key in production.
			continue
		}
		if capabilityAllowlist[name] || nonCapabilityFields[name] {
			continue
		}
		offending = append(offending, name)
	}
	sort.Strings(offending)
	return offending
}
