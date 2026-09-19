// Command plimsollctl is the operator CLI. It connects as plimsoll_owner and is run by
// hand, exactly like goose -- it is never reachable over HTTP.
//
// Minting invites is an administrative act, so it lives here rather than behind an
// endpoint. Giving the request-serving role a path to create accounts would put an admin
// backdoor in the process most exposed to the internet, for no gain: there is no invite
// UI in V1 (K16).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/store"
)

// ownerDSNEnv is where every subcommand finds its connection. The owner role, never the
// app role: this tool does DDL and mints invites, and the request-serving role must hold
// neither power (K15, K16).
const ownerDSNEnv = "PLIMSOLL_OWNER_DSN"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "plimsollctl:", err)
		os.Exit(1)
	}
}

// run dispatches on the subcommand. The subcommand is split off before flag parsing:
// flag.Parse stops at the first non-flag argument, so passing it the whole slice would
// silently ignore every flag that follows the subcommand.
func run(args []string) error {
	const usage = "usage: plimsollctl <invite|record|migrate> ...\n" +
		"  plimsollctl invite -email <address> [-ttl 168h]\n" +
		"  " + recordUsage + "\n" +
		"  " + migrateUsage

	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "migrate":
		return runMigrate()
	case "invite":
		return runInvite(args[1:])
	case "record":
		ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
		defer cancel()
		return runRecord(ctx, args[1:])
	default:
		return errors.New(usage)
	}
}

func runInvite(args []string) error {
	const usage = "usage: plimsollctl invite -email <address> [-ttl 168h]"

	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	email := fs.String("email", "", "email address the invite is bound to")
	ttl := fs.Duration("ttl", 7*24*time.Hour, "how long the invite stays valid")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New(usage)
	}

	dsn := os.Getenv(ownerDSNEnv)
	if dsn == "" {
		return fmt.Errorf("%s is not set", ownerDSNEnv)
	}

	ctx := context.Background()
	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	plain, hash, err := auth.NewOpaqueToken()
	if err != nil {
		return err
	}
	expires := time.Now().Add(*ttl)
	if err := store.New(pool).CreateInvite(ctx, store.CreateInviteParams{
		TokenHash: hash,
		Email:     *email,
		ExpiresAt: expires,
	}); err != nil {
		return fmt.Errorf("insert invite for %s: %w", *email, err)
	}

	// Printed once, to a human, on purpose: only the hash is stored, so this is the only
	// moment the token exists in readable form. It is never logged (L13).
	fmt.Printf("invite for %s\nexpires %s\ntoken   %s\n",
		*email, expires.Format(time.RFC3339), plain)
	return nil
}
