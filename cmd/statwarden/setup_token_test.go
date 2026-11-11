package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/api"
	"github.com/Ariazonaa/statwarden/internal/config"
)

// A fresh instance has to tell its operator the setup token, and the log is
// the one place only the operator can read. Once an account exists the token
// opens nothing, so printing it again would only put a secret in the log.
func TestSetupTokenIsLoggedOnlyWhileNoAccountExists(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	store := api.NewMemStore()

	tok := firstRunSetupToken(context.Background(), &config.Config{}, store, logger)
	if len(tok) != 32 {
		t.Fatalf("generated setup token %q, want 32 hex characters", tok)
	}
	if !strings.Contains(buf.String(), tok) {
		t.Fatalf("the generated token is not in the startup log:\n%s", buf.String())
	}

	if _, err := store.CreateInitialAdmin(context.Background(), "admin@example.com", "admin", "hash", "Admin"); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	tok = firstRunSetupToken(context.Background(), &config.Config{}, store, logger)
	if tok == "" {
		t.Fatal("a token must still be returned; an empty one would make setup answer 503 instead of 409")
	}
	if strings.Contains(buf.String(), tok) {
		t.Fatalf("the token was logged although an account exists:\n%s", buf.String())
	}
}

// A configured SETUP_TOKEN is used as given and never echoed into the log:
// the operator already knows it, and logs travel further than env files.
func TestConfiguredSetupTokenIsUsedAndNotLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := &config.Config{SetupToken: "provisioning-pass-for-ci"}

	tok := firstRunSetupToken(context.Background(), cfg, api.NewMemStore(), logger)
	if tok != cfg.SetupToken {
		t.Fatalf("token = %q, want the configured one", tok)
	}
	if strings.Contains(buf.String(), cfg.SetupToken) {
		t.Fatalf("the configured token was echoed into the log:\n%s", buf.String())
	}
}
