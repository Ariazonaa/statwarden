package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"

	"github.com/Ariazonaa/statwarden/internal/api"
	"github.com/Ariazonaa/statwarden/internal/config"
)

// firstRunSetupToken returns the secret POST /api/v1/setup requires.
//
// Until the first account exists, setup makes its caller an admin, and the
// instance is reachable from the moment it starts — so without a secret the
// operator races everyone else on the network for their own server. The token
// is printed in the startup log because that is the one channel only the
// operator can read. SETUP_TOKEN replaces it for headless provisioning and is
// not echoed, since whoever set it already knows it.
//
// A token is returned even when accounts exist: the handler answers 409 for a
// completed setup before it looks at the token, and an empty token would make
// it answer 503 instead.
func firstRunSetupToken(ctx context.Context, cfg *config.Config, store api.Store, logger *slog.Logger) string {
	if cfg.SetupToken != "" {
		return cfg.SetupToken
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Without randomness there is no safe token; an empty one keeps setup
		// closed (503) rather than open.
		logger.Error("cannot generate a setup token; first-run setup is unavailable", "error", err)
		return ""
	}
	tok := hex.EncodeToString(buf)
	n, err := store.CountUsers(ctx)
	if err != nil {
		// Logged regardless: if the count cannot be read, the operator may
		// still need the token, and an instance that cannot count its users
		// is not one anybody else can set up either.
		logger.Warn("could not check for existing accounts", "error", err)
	}
	if err != nil || n == 0 {
		logger.Warn("first-run setup token: enter it on the setup page to create the admin account",
			"setup_token", tok)
	}
	return tok
}
