package bybit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// QueryAPI returns what this key is allowed to do. Documented as reachable "with any
// permission", so a read-only key can ask about itself (B5).
func (c *Client) QueryAPI(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, request{path: "/v5/user/query-api", cost: requestCost})
}

var (
	// ErrMalformedKeyInfo means the payload could not be read at all. Distinct from a
	// rejection: a key we could not assess is not a key we assessed and refused.
	ErrMalformedKeyInfo = errors.New("bybit: could not read the key's permissions")

	// ErrNotReadOnly means the venue itself says the key can write.
	ErrNotReadOnly = errors.New("bybit: this key is not read-only")

	// ErrOverPermissioned means the key holds a permission beyond the allowlist.
	ErrOverPermissioned = errors.New("bybit: this key holds more than reading")
)

// permissionAllowlist is every permission string a key may hold and still be accepted.
//
// A closed allowlist rather than a denylist, for the same reason Binance's is: a venue that
// adds a permission would have it accepted by default, and a permission we have never heard
// of is rejected until someone decides otherwise (K9, L13).
//
// ExchangeHistory is the only entry, and it reads. Everything else Bybit documents --
// SpotTrade, Order, Position, Withdraw, AccountTransfer, OptionsTrade, DerivativesTrade,
// SubMemberTransfer -- either trades or moves money.
var permissionAllowlist = map[string]bool{
	"ExchangeHistory": true,
}

// KeyInfo is what a verified key turned out to be.
type KeyInfo struct {
	ReadOnly bool

	// Granted is every permission string the key holds, sorted, so a caller can report what
	// it found rather than only that it refused.
	Granted []string
}

// ParsePermissions decodes the key info and applies the policy.
//
// Two independent checks, either of which rejects: `readOnly == 1` is the VENUE's promise
// that the key cannot write, and the allowlist is OURS about what it may hold. Neither
// subsumes the other -- a key could be read-only today and have a permission that becomes
// writable when Bybit changes what it means, and a key with no listed permission could still
// be read/write (B5).
//
// Exported separately from the client so the policy is tested against fixtures rather than a
// network, which is where every case that matters lives.
func ParsePermissions(raw json.RawMessage) (KeyInfo, error) {
	var payload struct {
		ReadOnly    *int                `json:"readOnly"`
		Permissions map[string][]string `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return KeyInfo{}, fmt.Errorf("%w: %w", ErrMalformedKeyInfo, err)
	}
	if payload.ReadOnly == nil {
		// Absent is not false. A payload missing the one field the decision rests on is a
		// payload we cannot decide from, and defaulting it to "read-only" would accept every
		// key whose shape we failed to parse.
		return KeyInfo{}, fmt.Errorf("%w: the response does not say whether the key is read-only",
			ErrMalformedKeyInfo)
	}

	info := KeyInfo{ReadOnly: *payload.ReadOnly == 1}

	var offending []string
	for _, granted := range payload.Permissions {
		for _, name := range granted {
			info.Granted = append(info.Granted, name)
			if !permissionAllowlist[name] {
				offending = append(offending, name)
			}
		}
	}
	sort.Strings(info.Granted)
	sort.Strings(offending)

	if len(offending) > 0 {
		return KeyInfo{}, fmt.Errorf(
			"%w: turn off %s on this key, or issue a separate read-only key",
			ErrOverPermissioned, strings.Join(offending, ", "))
	}
	if !info.ReadOnly {
		return KeyInfo{}, fmt.Errorf(
			"%w: the venue reports readOnly=0, so it can write even with no permission listed",
			ErrNotReadOnly)
	}
	return info, nil
}
