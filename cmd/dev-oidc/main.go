// Command dev-oidc runs a lightweight dummy OAuth2/OIDC identity server for local development and CI E2E testing.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wichert/minicrm/internal/auth/devidm"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("dev_oidc_fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	addr := flag.String("addr", envOrDefault("DEV_OIDC_ADDR", "127.0.0.1:9000"), "HTTP listen address")
	issuer := flag.String("issuer", envOrDefault("DEV_OIDC_ISSUER_URL", ""), "OIDC issuer URL (defaults to http://<addr>)")
	clientID := flag.String("client-id", envOrDefault("DEV_OIDC_CLIENT_ID", ""), "Optional expected OAuth2 client_id")
	clientSecret := flag.String("client-secret", envOrDefault("DEV_OIDC_CLIENT_SECRET", ""), "Optional expected OAuth2 client_secret")
	sub := flag.String("sub", envOrDefault("DEV_OIDC_SUB", "dev-admin-sub"), "Fixed OIDC subject (sub) claim")
	email := flag.String("email", envOrDefault("DEV_OIDC_EMAIL", "admin@example.com"), "Fixed OIDC email claim")
	name := flag.String("name", envOrDefault("DEV_OIDC_NAME", "Dev Admin"), "Fixed OIDC name claim")
	flag.Parse()

	issuerURL := strings.TrimSpace(*issuer)
	if issuerURL == "" {
		issuerURL = "http://" + strings.TrimSpace(*addr)
	}

	idm, err := devidm.NewServer(devidm.Config{
		IssuerURL:    issuerURL,
		ClientID:     *clientID,
		ClientSecret: *clientSecret,
		Subject:      *sub,
		Email:        *email,
		Name:         *name,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           idm.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("dev_oidc_listening",
		slog.String("addr", *addr),
		slog.String("issuer_url", issuerURL),
		slog.String("sub", *sub),
		slog.String("email", *email),
		slog.String("name", *name),
	)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
