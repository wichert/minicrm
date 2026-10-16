// Package config loads and validates application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config holds the runtime configuration for MiniCRM.
type Config struct {
	HTTPAddr           string `env:"MINICRM_HTTP_ADDR" envDefault:":8080"`
	DatabaseURL        string `env:"MINICRM_DATABASE_URL,required"`
	AttachmentsDir     string `env:"MINICRM_ATTACHMENTS_DIR" envDefault:"./data/attachments"`
	BootstrapAdminMail string `env:"MINICRM_BOOTSTRAP_ADMIN_EMAIL,required"`
	BootstrapAdminName string `env:"MINICRM_BOOTSTRAP_ADMIN_NAME" envDefault:"Admin"`
	DisplayTimezone    string `env:"MINICRM_DISPLAY_TIMEZONE" envDefault:"UTC"`
	MailDomain         string `env:"MINICRM_MAIL_DOMAIN" envDefault:"minicrm.local"`
	CookieSecure       bool   `env:"MINICRM_COOKIE_SECURE" envDefault:"true"`

	// IMAP configuration (username + password authentication)
	IMAPHost         string        `env:"MINICRM_IMAP_HOST,required"`
	IMAPPort         int           `env:"MINICRM_IMAP_PORT" envDefault:"993"`
	IMAPUsername     string        `env:"MINICRM_IMAP_USERNAME,required"`
	IMAPPassword     string        `env:"MINICRM_IMAP_PASSWORD,required"`
	IMAPImplicitTLS  bool          `env:"MINICRM_IMAP_IMPLICIT_TLS" envDefault:"true"`
	IMAPPollInterval time.Duration `env:"MINICRM_IMAP_POLL_INTERVAL" envDefault:"2m"`

	// SMTP configuration (username + password authentication)
	SMTPHost        string `env:"MINICRM_SMTP_HOST,required"`
	SMTPPort        int    `env:"MINICRM_SMTP_PORT" envDefault:"587"`
	SMTPUsername    string `env:"MINICRM_SMTP_USERNAME,required"`
	SMTPPassword    string `env:"MINICRM_SMTP_PASSWORD,required"`
	SMTPFromAddress string `env:"MINICRM_SMTP_FROM_ADDRESS,required"`
	SMTPFromName    string `env:"MINICRM_SMTP_FROM_NAME" envDefault:"MiniCRM Support"`
	SMTPTLSMode     string `env:"MINICRM_SMTP_TLS_MODE" envDefault:"starttls"` // "starttls", "implicit", "none"

	// OAuth2 / OIDC Identity Server configuration
	OIDCIssuerURL    string `env:"MINICRM_OIDC_ISSUER_URL,required"`
	OIDCClientID     string `env:"MINICRM_OIDC_CLIENT_ID,required"`
	OIDCClientSecret string `env:"MINICRM_OIDC_CLIENT_SECRET,required"`
	OIDCRedirectURL  string `env:"MINICRM_OIDC_REDIRECT_URL,required"`
}

// Load parses environment variables and validates the resulting configuration.
func Load() (*Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return nil, fmt.Errorf("parse config from env: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks semantic constraints on Config values.
func (c *Config) Validate() error {
	if c.DisplayTimezone == "" {
		return errors.New("display timezone must not be empty")
	}
	if _, err := time.LoadLocation(c.DisplayTimezone); err != nil {
		return fmt.Errorf("invalid MINICRM_DISPLAY_TIMEZONE %q: %w", c.DisplayTimezone, err)
	}
	if c.IMAPPort <= 0 || c.IMAPPort > 65535 {
		return fmt.Errorf("invalid MINICRM_IMAP_PORT: %d", c.IMAPPort)
	}
	if c.SMTPPort <= 0 || c.SMTPPort > 65535 {
		return fmt.Errorf("invalid MINICRM_SMTP_PORT: %d", c.SMTPPort)
	}
	switch c.SMTPTLSMode {
	case "starttls", "implicit", "none":
	default:
		return fmt.Errorf("invalid MINICRM_SMTP_TLS_MODE %q (want starttls, implicit, or none)", c.SMTPTLSMode)
	}
	return nil
}
