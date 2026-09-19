package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/exchange/binance"
	"github.com/Contictus/plimsoll/backend/internal/integration"
	"github.com/Contictus/plimsoll/backend/internal/ratelimit"
	"github.com/google/uuid"
)

// Environment variables the recorder reads its key from. Deliberately not the database:
// recording is done by hand, before an integration exists, and a tool that can decrypt
// stored credentials is a tool that can be pointed at someone else's.
const (
	envAPIKey    = "PLIMSOLL_BINANCE_API_KEY"
	envAPISecret = "PLIMSOLL_BINANCE_API_SECRET"
)

// bootstrapWeightPerMinute is the budget used for the single unsigned exchangeInfo call
// that discovers the real ceiling. It is deliberately far below any published limit: the
// real number comes from the exchange (K24), and this one only has to be safe enough to
// ask what it is.
const bootstrapWeightPerMinute = 100

// recordableEndpoints maps a name an operator types to the call it makes. Only read
// endpoints exist here, and only read endpoints exist in the client at all (L13).
var recordableEndpoints = []string{
	"exchangeInfo", "account", "apiRestrictions", "myTrades", "deposits", "withdrawals",
}

const recordUsage = "usage: plimsollctl record -endpoint <" +
	"exchangeInfo|account|apiRestrictions|myTrades|deposits|withdrawals> " +
	"[-symbol BTCUSDT] [-from-id 0] [-limit 1000] [-out testdata/fixtures/binance]"

// runRecord captures one live response into a fixture. Development runs against the file
// it writes, never against the live API (CLAUDE.md section 2), so this is the only place
// in the repo that talks to Binance for real.
//
// Redaction happens in the write path here, and the bytes are checked for the credential
// once more immediately before the file is created. Recording first and cleaning up later
// is how a secret reaches git history, where deleting it does not remove it.
func runRecord(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "", "which endpoint to record: "+strings.Join(recordableEndpoints, ", "))
	symbol := fs.String("symbol", "", "symbol, required for myTrades")
	fromID := fs.String("from-id", "", "myTrades fromId; empty means unset, \"0\" means from the beginning")
	limit := fs.Int("limit", 0, "page size; zero leaves the exchange's default")
	out := fs.String("out", filepath.Join("testdata", "fixtures", "binance"), "directory to write into")
	baseURL := fs.String("base-url", "https://api.binance.com", "exchange base url")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *endpoint == "" {
		return errors.New(recordUsage)
	}

	apiKey, apiSecret := os.Getenv(envAPIKey), os.Getenv(envAPISecret)
	if apiKey == "" || apiSecret == "" {
		return fmt.Errorf("set %s and %s to a read-only key", envAPIKey, envAPISecret)
	}
	cred := integration.Credential{
		APIKey:    auth.Secret(apiKey),
		APISecret: auth.Secret(apiSecret),
	}

	client, err := connectForRecording(ctx, cred, *baseURL)
	if err != nil {
		return err
	}

	payload, err := fetch(ctx, client, *endpoint, *symbol, *fromID, *limit)
	if err != nil {
		return fmt.Errorf("record %s: %w", *endpoint, err)
	}

	path, err := writeFixture(payload, *endpoint, *symbol, *out, apiKey, apiSecret)
	if err != nil {
		return err
	}
	fmt.Printf("recorded %s -> %s (%d bytes, redacted)\n", *endpoint, path, len(payload))
	return nil
}

// connectForRecording builds a client whose budget came from the exchange, and refuses a
// key that can do more than read. K9 applies to an operator's own key too: a recorder that
// accepts a trading key is a trading key sitting in a shell history.
func connectForRecording(
	ctx context.Context, cred integration.Credential, baseURL string,
) (*binance.Client, error) {
	newClient := func(perMinute int) (*binance.Client, error) {
		limiter, err := ratelimit.New(ratelimit.Config{
			SharedPerMinute:         perMinute,
			PerIntegrationPerMinute: perMinute,
		}, ratelimit.SystemClock{})
		if err != nil {
			return nil, err
		}
		return binance.New(binance.Config{
			IntegrationID: uuid.New(), // one throwaway budget; nothing here is persisted
			Credential:    cred,
			Limiter:       limiter,
			BaseURL:       baseURL,
		})
	}

	bootstrap, err := newClient(bootstrapWeightPerMinute)
	if err != nil {
		return nil, err
	}
	info, err := bootstrap.ExchangeInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("read exchangeInfo: %w", err)
	}
	perMinute, err := binance.RequestWeightPerMinute(info)
	if err != nil {
		return nil, err
	}

	client, err := newClient(perMinute)
	if err != nil {
		return nil, err
	}
	restrictions, err := client.APIRestrictions(ctx)
	if err != nil {
		return nil, fmt.Errorf("read api key permissions: %w", err)
	}
	if _, err := integration.ParsePermissions(restrictions); err != nil {
		return nil, fmt.Errorf("this key may not be used: %w", err)
	}
	return client, nil
}

func fetch(
	ctx context.Context, client *binance.Client, endpoint, symbol, fromID string, limit int,
) (json.RawMessage, error) {
	switch endpoint {
	case "exchangeInfo":
		return client.ExchangeInfo(ctx)
	case "account":
		return client.Account(ctx)
	case "apiRestrictions":
		return client.APIRestrictions(ctx)
	case "myTrades":
		if symbol == "" {
			return nil, fmt.Errorf("myTrades needs -symbol; there is no all-trades endpoint (F4)")
		}
		query := binance.MyTradesQuery{Symbol: symbol, Limit: limit}
		if fromID != "" {
			// Parsed from a string so that "0" -- the F5 probe -- is distinguishable from
			// the flag being left unset.
			parsed, err := strconv.ParseInt(fromID, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("-from-id must be a whole number: %w", err)
			}
			query.FromID = &parsed
		}
		return client.MyTrades(ctx, query)
	case "deposits":
		return client.DepositHistory(ctx, binance.HistoryQuery{Limit: limit})
	case "withdrawals":
		return client.WithdrawHistory(ctx, binance.HistoryQuery{Limit: limit})
	default:
		return nil, fmt.Errorf("unknown endpoint %q; known: %s",
			endpoint, strings.Join(recordableEndpoints, ", "))
	}
}

// writeFixture redacts, checks the result for the credential, and only then creates the
// file. The order is the point: nothing unredacted is ever written, not even briefly.
func writeFixture(
	payload json.RawMessage, endpoint, symbol, dir, apiKey, apiSecret string,
) (string, error) {
	fixture, err := binance.Fixture(payload, endpoint)
	if err != nil {
		return "", err
	}
	if err := binance.AssertNoSecrets(fixture, apiKey, apiSecret); err != nil {
		return "", err
	}

	name := strings.ToLower(endpoint)
	if symbol != "" {
		name += "_" + strings.ToLower(symbol)
	}
	path := filepath.Join(dir, name+".json")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(path, append(fixture, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// recordTimeout bounds one recording. An operator waiting on a hung connection has no way
// to tell it from an empty account.
const recordTimeout = 2 * time.Minute
