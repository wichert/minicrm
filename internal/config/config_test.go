package config_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/wichert/minicrm/internal/config"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MINICRM_DATABASE_URL", "postgres://localhost:5432/minicrm?sslmode=disable")
	t.Setenv("MINICRM_BOOTSTRAP_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("MINICRM_OIDC_ISSUER_URL", "https://id.example.com")
	t.Setenv("MINICRM_OIDC_CLIENT_ID", "minicrm")
	t.Setenv("MINICRM_OIDC_CLIENT_SECRET", "oidc-secret")
	t.Setenv("MINICRM_OIDC_REDIRECT_URL", "https://crm.example.com/auth/callback")
	setAccountEnv(t, 1, "support")
}

// setAccountEnv configures a single numbered mail account.
func setAccountEnv(t *testing.T, index int, id string) {
	t.Helper()
	prefix := "MINICRM_ACCOUNT_" + strconv.Itoa(index) + "_"
	t.Setenv(prefix+"ID", id)
	t.Setenv(prefix+"NAME", strings.ToUpper(id))
	t.Setenv(prefix+"OIDC_GROUP", "crm-"+id)
	t.Setenv(prefix+"IMAP_HOST", "imap.example.com")
	t.Setenv(prefix+"IMAP_USERNAME", id+"@example.com")
	t.Setenv(prefix+"IMAP_PASSWORD", "secret")
	t.Setenv(prefix+"SMTP_HOST", "smtp.example.com")
	t.Setenv(prefix+"SMTP_USERNAME", id+"@example.com")
	t.Setenv(prefix+"SMTP_PASSWORD", "secret")
	t.Setenv(prefix+"SMTP_FROM_ADDRESS", id+"@example.com")
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
	if len(cfg.Accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(cfg.Accounts))
	}
	acct := cfg.Accounts[0]
	if acct.ID != "support" || acct.Name != "SUPPORT" || acct.OIDCGroup != "crm-support" {
		t.Fatalf("unexpected account: %+v", acct)
	}
	if acct.IMAPPort != 993 {
		t.Fatalf("got default IMAP port %d, want 993", acct.IMAPPort)
	}
	if acct.SMTPPort != 587 {
		t.Fatalf("got default SMTP port %d, want 587", acct.SMTPPort)
	}
	if acct.SMTPTLSMode != "starttls" {
		t.Fatalf("got default SMTP TLS mode %q, want starttls", acct.SMTPTLSMode)
	}
	if cfg.OIDCGroupsClaim != "groups" {
		t.Fatalf("got default OIDC groups claim %q, want groups", cfg.OIDCGroupsClaim)
	}
}

func TestLoadTwoAccounts(t *testing.T) {
	setRequiredEnv(t)
	setAccountEnv(t, 2, "sales")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("got %d accounts, want 2", len(cfg.Accounts))
	}
	if cfg.Accounts[0].ID != "support" || cfg.Accounts[1].ID != "sales" {
		t.Fatalf("unexpected accounts: %+v", cfg.Accounts)
	}
	if _, ok := cfg.AccountByID("sales"); !ok {
		t.Fatal("expected AccountByID to find sales account")
	}
	if _, ok := cfg.AccountByID("nope"); ok {
		t.Fatal("did not expect AccountByID to find unknown account")
	}
}

func TestLoadNAccounts(t *testing.T) {
	setRequiredEnv(t)
	setAccountEnv(t, 2, "sales")
	setAccountEnv(t, 3, "billing")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if len(cfg.Accounts) != 3 {
		t.Fatalf("got %d accounts, want 3", len(cfg.Accounts))
	}
}

func TestLoadCustomOIDCGroupsClaim(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MINICRM_OIDC_GROUPS_CLAIM", "roles")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.OIDCGroupsClaim != "roles" {
		t.Fatalf("got groups claim %q, want roles", cfg.OIDCGroupsClaim)
	}
}

func TestLoadOIDCScopes(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if got, want := strings.Join(cfg.OIDCScopes, ","), "openid,profile,email"; got != want {
		t.Fatalf("got default scopes %q, want %q", got, want)
	}

	t.Setenv("MINICRM_OIDC_SCOPES", "openid,profile,email,groups_name")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if got, want := strings.Join(cfg.OIDCScopes, ","), "openid,profile,email,groups_name"; got != want {
		t.Fatalf("got scopes %q, want %q", got, want)
	}
}

func TestLoadOIDCScopesRequireOpenID(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MINICRM_OIDC_SCOPES", "profile,email")

	if _, err := config.Load(); err == nil {
		t.Fatal("expected error when openid scope is missing, got nil")
	}
}

func TestLoadInvalidTimezone(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MINICRM_DISPLAY_TIMEZONE", "Not/A_Timezone")

	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for invalid display timezone, got nil")
	}
}

func TestLoadMissingAccountID(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MINICRM_ACCOUNT_1_ID", "")

	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for empty account ID, got nil")
	}
}

func TestLoadDuplicateAccountID(t *testing.T) {
	setRequiredEnv(t)
	setAccountEnv(t, 2, "support")

	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for duplicate account IDs, got nil")
	}
}

func TestLoadMissingOIDCGroup(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MINICRM_ACCOUNT_1_OIDC_GROUP", "")

	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for missing OIDC group, got nil")
	}
}

func TestLoadInvalidAccountID(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MINICRM_ACCOUNT_1_ID", "Not Valid!")

	if _, err := config.Load(); err == nil {
		t.Fatal("expected error for invalid account ID, got nil")
	}
}

func TestLoadInvalidPorts(t *testing.T) {
	t.Run("imap", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MINICRM_ACCOUNT_1_IMAP_PORT", "70000")
		if _, err := config.Load(); err == nil {
			t.Fatal("expected error for invalid IMAP port, got nil")
		}
	})
	t.Run("smtp", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MINICRM_ACCOUNT_1_SMTP_TLS_MODE", "bogus")
		if _, err := config.Load(); err == nil {
			t.Fatal("expected error for invalid SMTP TLS mode, got nil")
		}
	})
}

func TestIMAPInitialSyncDays(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Accounts[0].IMAPInitialSyncDays; got != 60 {
		t.Fatalf("default initial sync days = %d, want 60", got)
	}

	t.Setenv("MINICRM_ACCOUNT_1_IMAP_INITIAL_SYNC_DAYS", "0")
	cfg, err = config.Load()
	if err != nil || cfg.Accounts[0].IMAPInitialSyncDays != 0 {
		t.Fatalf("0 (full backfill) must be accepted: %v %+v", err, cfg)
	}

	t.Setenv("MINICRM_ACCOUNT_1_IMAP_INITIAL_SYNC_DAYS", "-5")
	if _, err := config.Load(); err == nil {
		t.Fatal("negative initial sync days must be rejected")
	}
}

func TestAppBrandingAndSessionConfig(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if cfg.AppName != "MiniCRM" || cfg.AppShortName != "MiniCRM" {
		t.Fatalf("unexpected default app name/short name: %q / %q", cfg.AppName, cfg.AppShortName)
	}
	if cfg.AppDescription != "Lightweight multi-account email CRM" {
		t.Fatalf("unexpected default description: %q", cfg.AppDescription)
	}
	if cfg.SessionLifetime.Hours() != 168 {
		t.Fatalf("got default session lifetime %v, want 168h", cfg.SessionLifetime)
	}
	if cfg.SessionIdleTimeout != 0 {
		t.Fatalf("got default session idle timeout %v, want 0", cfg.SessionIdleTimeout)
	}

	// Custom branding with short_name defaulting to AppName when unset.
	t.Setenv("MINICRM_APP_NAME", "Acme Support Desk")
	t.Setenv("MINICRM_APP_DESCRIPTION", "Acme customer inbox")
	t.Setenv("MINICRM_SESSION_LIFETIME", "48h")
	t.Setenv("MINICRM_SESSION_IDLE_TIMEOUT", "12h")
	t.Setenv("MINICRM_SESSION_ENCRYPTION_KEY", "super-secret-session-encryption-key")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("Load custom: %v", err)
	}
	if cfg.AppName != "Acme Support Desk" || cfg.AppShortName != "Acme Support Desk" {
		t.Fatalf("expected short name to default to AppName, got %q / %q", cfg.AppName, cfg.AppShortName)
	}
	if cfg.AppDescription != "Acme customer inbox" {
		t.Fatalf("got description %q", cfg.AppDescription)
	}
	if cfg.SessionLifetime.Hours() != 48 || cfg.SessionIdleTimeout.Hours() != 12 {
		t.Fatalf("unexpected session timeouts: %v / %v", cfg.SessionLifetime, cfg.SessionIdleTimeout)
	}
	if cfg.SessionEncryptionKey != "super-secret-session-encryption-key" {
		t.Fatalf("unexpected encryption key: %q", cfg.SessionEncryptionKey)
	}

	// Explicit short name
	t.Setenv("MINICRM_APP_SHORT_NAME", "AcmeCRM")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("Load explicit short name: %v", err)
	}
	if cfg.AppShortName != "AcmeCRM" {
		t.Fatalf("got short name %q, want AcmeCRM", cfg.AppShortName)
	}

	// Validation failures
	t.Run("empty_app_name", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MINICRM_APP_NAME", "   ")
		if _, err := config.Load(); err == nil {
			t.Fatal("expected error for empty MINICRM_APP_NAME")
		}
	})
	t.Run("non_positive_session_lifetime", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MINICRM_SESSION_LIFETIME", "0s")
		if _, err := config.Load(); err == nil {
			t.Fatal("expected error for 0s session lifetime")
		}
	})
	t.Run("negative_idle_timeout", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MINICRM_SESSION_IDLE_TIMEOUT", "-1m")
		if _, err := config.Load(); err == nil {
			t.Fatal("expected error for negative idle timeout")
		}
	})
	t.Run("idle_exceeds_lifetime", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MINICRM_SESSION_LIFETIME", "2h")
		t.Setenv("MINICRM_SESSION_IDLE_TIMEOUT", "4h")
		if _, err := config.Load(); err == nil {
			t.Fatal("expected error when idle timeout exceeds lifetime")
		}
	})
}
