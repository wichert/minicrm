// Package config loads and validates application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// MailAccountConfig describes a single IMAP/SMTP account monitored by MiniCRM.
//
// Accounts are configured using numbered environment variable prefixes, e.g.
// MINICRM_ACCOUNT_1_ID, MINICRM_ACCOUNT_1_IMAP_HOST, MINICRM_ACCOUNT_2_ID, and so on.
type MailAccountConfig struct {
	// ID is the mandatory, stable, unique identifier for the account (e.g. "support").
	ID string `env:"ID,required"`
	// Name is the mandatory human-readable account name shown in the UI.
	Name string `env:"NAME,required"`
	// OIDCGroup is the mandatory OIDC group/role claim value granting access to this account.
	OIDCGroup string `env:"OIDC_GROUP,required"`

	// IMAP configuration (username + password authentication)
	IMAPHost         string        `env:"IMAP_HOST,required"`
	IMAPPort         int           `env:"IMAP_PORT" envDefault:"993"`
	IMAPUsername     string        `env:"IMAP_USERNAME,required"`
	IMAPPassword     string        `env:"IMAP_PASSWORD,required"`
	IMAPImplicitTLS  bool          `env:"IMAP_IMPLICIT_TLS" envDefault:"true"`
	IMAPPollInterval time.Duration `env:"IMAP_POLL_INTERVAL" envDefault:"2m"`
	// IMAPInitialSyncDays limits the first synchronization of a new account to mail received in
	// the last N days. Set to 0 to backfill the complete mailbox history.
	IMAPInitialSyncDays int `env:"IMAP_INITIAL_SYNC_DAYS" envDefault:"60"`

	// SMTP configuration (username + password authentication)
	SMTPHost        string `env:"SMTP_HOST,required"`
	SMTPPort        int    `env:"SMTP_PORT" envDefault:"587"`
	SMTPUsername    string `env:"SMTP_USERNAME,required"`
	SMTPPassword    string `env:"SMTP_PASSWORD,required"`
	SMTPFromAddress string `env:"SMTP_FROM_ADDRESS,required"`
	SMTPFromName    string `env:"SMTP_FROM_NAME" envDefault:"MiniCRM Support"`
	SMTPTLSMode     string `env:"SMTP_TLS_MODE" envDefault:"starttls"` // "starttls", "implicit", "none"
}

// AccountEnvPrefixFormat is the printf format for a mail account's env var prefix.
const AccountEnvPrefixFormat = "MINICRM_ACCOUNT_%d_"

// accountIDPattern restricts account IDs to slug-like values, as they are used as
// database keys and in HTML form field names.
var accountIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Config holds the runtime configuration for MiniCRM.
type Config struct {
	HTTPAddr           string `env:"MINICRM_HTTP_ADDR" envDefault:":8080"`
	DatabaseURL        string `env:"MINICRM_DATABASE_URL,required"`
	AttachmentsDir     string `env:"MINICRM_ATTACHMENTS_DIR" envDefault:"./data/attachments"`
	BootstrapAdminMail string `env:"MINICRM_BOOTSTRAP_ADMIN_EMAIL,required"`
	BootstrapAdminName string `env:"MINICRM_BOOTSTRAP_ADMIN_NAME" envDefault:"Admin"`
	DisplayTimezone    string `env:"MINICRM_DISPLAY_TIMEZONE" envDefault:"UTC"`
	InboxPageSize      int    `env:"MINICRM_INBOX_PAGE_SIZE" envDefault:"100"`
	MailDomain         string `env:"MINICRM_MAIL_DOMAIN" envDefault:"minicrm.local"`
	CookieSecure       bool   `env:"MINICRM_COOKIE_SECURE" envDefault:"true"`

	// Per-instance PWA / application branding
	AppName        string `env:"MINICRM_APP_NAME" envDefault:"MiniCRM"`
	AppShortName   string `env:"MINICRM_APP_SHORT_NAME"`
	AppDescription string `env:"MINICRM_APP_DESCRIPTION" envDefault:"Lightweight multi-account email CRM"`

	// Session management & refresh token encryption
	SessionLifetime      time.Duration `env:"MINICRM_SESSION_LIFETIME" envDefault:"168h"`
	SessionIdleTimeout   time.Duration `env:"MINICRM_SESSION_IDLE_TIMEOUT" envDefault:"0s"`
	SessionEncryptionKey string        `env:"MINICRM_SESSION_ENCRYPTION_KEY"`

	// Mail accounts (loaded from MINICRM_ACCOUNT_<N>_* env prefixes).
	Accounts []MailAccountConfig `env:"-"`

	// OAuth2 / OIDC Identity Server configuration
	OIDCIssuerURL    string `env:"MINICRM_OIDC_ISSUER_URL,required"`
	OIDCClientID     string `env:"MINICRM_OIDC_CLIENT_ID,required"`
	OIDCClientSecret string `env:"MINICRM_OIDC_CLIENT_SECRET,required"`
	OIDCRedirectURL  string `env:"MINICRM_OIDC_REDIRECT_URL,required"`
	// OIDCGroupsClaim names the ID token claim carrying the user's group memberships.
	OIDCGroupsClaim string `env:"MINICRM_OIDC_GROUPS_CLAIM" envDefault:"groups"`
	// OIDCScopes are the scopes requested from the OIDC provider. Providers such as
	// Kanidm only include the groups claim when a groups scope is requested
	// (e.g. "groups_name" for bare group names).
	OIDCScopes []string `env:"MINICRM_OIDC_SCOPES" envDefault:"openid,profile,email"`
}

// Load parses environment variables and validates the resulting configuration.
func Load() (*Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return nil, fmt.Errorf("parse config from env: %w", err)
	}
	cfg.AppName = strings.TrimSpace(cfg.AppName)
	cfg.AppShortName = strings.TrimSpace(cfg.AppShortName)
	if cfg.AppShortName == "" {
		cfg.AppShortName = cfg.AppName
	}
	cfg.AppDescription = strings.TrimSpace(cfg.AppDescription)
	accounts, err := loadAccounts()
	if err != nil {
		return nil, err
	}
	cfg.Accounts = accounts
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// loadAccounts parses the numbered MINICRM_ACCOUNT_<N>_* environment variable prefixes,
// starting at index 1 and stopping at the first index without a MINICRM_ACCOUNT_<N>_ID value.
func loadAccounts() ([]MailAccountConfig, error) {
	accounts := make([]MailAccountConfig, 0, 2)
	for i := 1; ; i++ {
		prefix := fmt.Sprintf(AccountEnvPrefixFormat, i)
		if _, ok := os.LookupEnv(prefix + "ID"); !ok {
			break
		}
		acct, err := env.ParseAsWithOptions[MailAccountConfig](env.Options{Prefix: prefix})
		if err != nil {
			return nil, fmt.Errorf("parse mail account %d (%s*): %w", i, prefix, err)
		}
		accounts = append(accounts, acct)
	}
	if len(accounts) == 0 {
		return nil, errors.New("at least one mail account must be configured (MINICRM_ACCOUNT_1_ID is required)")
	}
	return accounts, nil
}

// Validate checks semantic constraints on Config values.
func (c *Config) Validate() error {
	if c.InboxPageSize < 1 || c.InboxPageSize > 500 {
		return errors.New("MINICRM_INBOX_PAGE_SIZE must be between 1 and 500")
	}
	if strings.TrimSpace(c.AppName) == "" {
		return errors.New("MINICRM_APP_NAME must not be empty")
	}
	if c.SessionLifetime <= 0 {
		return errors.New("MINICRM_SESSION_LIFETIME must be positive")
	}
	if c.SessionIdleTimeout < 0 {
		return errors.New("MINICRM_SESSION_IDLE_TIMEOUT must not be negative")
	}
	if c.SessionIdleTimeout > c.SessionLifetime {
		return errors.New("MINICRM_SESSION_IDLE_TIMEOUT must not exceed MINICRM_SESSION_LIFETIME")
	}
	if c.DisplayTimezone == "" {
		return errors.New("display timezone must not be empty")
	}
	if _, err := time.LoadLocation(c.DisplayTimezone); err != nil {
		return fmt.Errorf("invalid MINICRM_DISPLAY_TIMEZONE %q: %w", c.DisplayTimezone, err)
	}
	if len(c.Accounts) == 0 {
		return errors.New("at least one mail account must be configured")
	}
	seenIDs := make(map[string]struct{}, len(c.Accounts))
	for i, acct := range c.Accounts {
		if acct.ID == "" {
			return fmt.Errorf("mail account %d: ID must not be empty", i+1)
		}
		if !accountIDPattern.MatchString(acct.ID) {
			return fmt.Errorf("mail account %d: invalid ID %q (want lowercase letters, digits, '-' or '_')", i+1, acct.ID)
		}
		if _, dup := seenIDs[acct.ID]; dup {
			return fmt.Errorf("duplicate mail account ID %q", acct.ID)
		}
		seenIDs[acct.ID] = struct{}{}
		if acct.Name == "" {
			return fmt.Errorf("mail account %q: NAME must not be empty", acct.ID)
		}
		if acct.OIDCGroup == "" {
			return fmt.Errorf("mail account %q: OIDC_GROUP must not be empty", acct.ID)
		}
		if acct.IMAPInitialSyncDays < 0 {
			return fmt.Errorf("mail account %q: IMAP_INITIAL_SYNC_DAYS must be 0 (full backfill) or positive", acct.ID)
		}
		if acct.IMAPHost == "" {
			return fmt.Errorf("mail account %q: IMAP_HOST must not be empty", acct.ID)
		}
		if acct.IMAPUsername == "" {
			return fmt.Errorf("mail account %q: IMAP_USERNAME must not be empty", acct.ID)
		}
		if acct.IMAPPort <= 0 || acct.IMAPPort > 65535 {
			return fmt.Errorf("mail account %q: invalid IMAP_PORT: %d", acct.ID, acct.IMAPPort)
		}
		if acct.SMTPHost == "" {
			return fmt.Errorf("mail account %q: SMTP_HOST must not be empty", acct.ID)
		}
		if acct.SMTPUsername == "" {
			return fmt.Errorf("mail account %q: SMTP_USERNAME must not be empty", acct.ID)
		}
		if acct.SMTPFromAddress == "" {
			return fmt.Errorf("mail account %q: SMTP_FROM_ADDRESS must not be empty", acct.ID)
		}
		if acct.SMTPPort <= 0 || acct.SMTPPort > 65535 {
			return fmt.Errorf("mail account %q: invalid SMTP_PORT: %d", acct.ID, acct.SMTPPort)
		}
		switch acct.SMTPTLSMode {
		case "starttls", "implicit", "none":
		default:
			return fmt.Errorf("mail account %q: invalid SMTP_TLS_MODE %q (want starttls, implicit, or none)", acct.ID, acct.SMTPTLSMode)
		}
	}
	if c.OIDCGroupsClaim == "" {
		return errors.New("MINICRM_OIDC_GROUPS_CLAIM must not be empty")
	}
	if len(c.OIDCScopes) == 0 {
		return errors.New("MINICRM_OIDC_SCOPES must not be empty")
	}
	seenScopes := make(map[string]struct{}, len(c.OIDCScopes))
	for _, scope := range c.OIDCScopes {
		if strings.TrimSpace(scope) == "" {
			return errors.New("MINICRM_OIDC_SCOPES must not contain empty scopes")
		}
		seenScopes[scope] = struct{}{}
	}
	if _, ok := seenScopes["openid"]; !ok {
		return errors.New(`MINICRM_OIDC_SCOPES must include the "openid" scope`)
	}
	return nil
}

// AccountByID returns the configured mail account with the given ID.
func (c *Config) AccountByID(id string) (MailAccountConfig, bool) {
	for _, acct := range c.Accounts {
		if acct.ID == id {
			return acct, true
		}
	}
	return MailAccountConfig{}, false
}
