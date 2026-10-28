module github.com/wichert/minicrm

go 1.27.2

require (
	github.com/alexedwards/scs/pgxstore v0.0.0-20251002162104-209de6e426de
	github.com/alexedwards/scs/v2 v2.9.0
	github.com/caarlos0/env/v11 v11.4.1
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/emersion/go-imap/v2 v2.0.0-beta.8
	github.com/emersion/go-message v0.18.2
	github.com/go-chi/chi/v5 v5.5.0
	github.com/go-jose/go-jose/v4 v4.1.6
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/microcosm-cc/bluemonday v1.0.27
	github.com/pressly/goose/v3 v3.29.0
	github.com/wneessen/go-mail v0.8.1
	golang.org/x/net v0.61.0
	golang.org/x/oauth2 v0.37.0
	golang.org/x/sync v0.24.0
)

require (
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/sethvargo/go-retry v0.5.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace example.com/seedcheck => /tmp/seedcheck
