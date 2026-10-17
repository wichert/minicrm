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
