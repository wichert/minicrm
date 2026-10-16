package config_test

import (
	"testing"

	"github.com/wichert/minicrm/internal/config"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MINICRM_DATABASE_URL", "postgres://localhost:5432/minicrm?sslmode=disable")
	t.Setenv("MINICRM_BOOTSTRAP_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("MINICRM_IMAP_HOST", "imap.example.com")
	t.Setenv("MINICRM_IMAP_USERNAME", "support@example.com")
	t.Setenv("MINICRM_IMAP_PASSWORD", "secret")
	t.Setenv("MINICRM_SMTP_HOST", "smtp.example.com")
	t.Setenv("MINICRM_SMTP_USERNAME", "support@example.com")
	t.Setenv("MINICRM_SMTP_PASSWORD", "secret")
	t.Setenv("MINICRM_SMTP_FROM_ADDRESS", "support@example.com")
	t.Setenv("MINICRM_OIDC_ISSUER_URL", "https://id.example.com")
	t.Setenv("MINICRM_OIDC_CLIENT_ID", "minicrm")
	t.Setenv("MINICRM_OIDC_CLIENT_SECRET", "oidc-secret")
	t.Setenv("MINICRM_OIDC_REDIRECT_URL", "https://crm.example.com/auth/callback")
}

func TestLoadValidConfig(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MINICRM_DISPLAY_TIMEZONE", "Europe/Amsterdam")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.DisplayTimezone != "Europe/Amsterdam" {
		t.Fatalf("got timezone %q, want Europe/Amsterdam", cfg.DisplayTimezone)
	}
	if cfg.IMAPPort != 993 {
		t.Fatalf("got default IMAP port %d, want 993", cfg.IMAPPort)
	}
}

func TestLoadInvalidTimezone(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MINICRM_DISPLAY_TIMEZONE", "Not/A_Timezone")

	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for invalid display timezone, got nil")
	}
}
