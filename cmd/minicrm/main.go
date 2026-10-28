// Command minicrm runs the MiniCRM HTTP server and background IMAP/SMTP workers.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wichert/minicrm/internal/assignment"
	"github.com/wichert/minicrm/internal/audit"
	"github.com/wichert/minicrm/internal/auth"
	"github.com/wichert/minicrm/internal/config"
	"github.com/wichert/minicrm/internal/db/migrations"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/mail"
	"github.com/wichert/minicrm/internal/storage"
	"github.com/wichert/minicrm/internal/threading"
	"github.com/wichert/minicrm/internal/web"
	"github.com/wichert/minicrm/internal/workflow"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("minicrm_fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	poolCfg.ConnConfig.Tracer = sqlc.NewStructuredQueryTracer(logger)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	sqlDB := stdlib.OpenDBFromPool(pool)
	if err := migrations.Up(ctx, sqlDB, cfg.Accounts[0].ID); err != nil {
		_ = sqlDB.Close()
		return err
	}
	_ = sqlDB.Close()

	queries := sqlc.New(pool)
	auditLogger := audit.NewLogger(queries, logger)

	attStore, err := storage.NewStore(cfg.AttachmentsDir, logger)
	if err != nil {
		return err
	}

	oidcProvider, err := auth.NewOIDCProvider(
		ctx,
		cfg.OIDCIssuerURL,
		cfg.OIDCClientID,
		cfg.OIDCClientSecret,
		cfg.OIDCRedirectURL,
		cfg.OIDCGroupsClaim,
		cfg.OIDCScopes,
	)
	if err != nil {
		return err
	}

	sessions := auth.NewSessionManagerWithTimeouts(cfg.CookieSecure, cfg.SessionLifetime, cfg.SessionIdleTimeout)
	sessions.Store = pgxstore.New(pool)

	tokenCipher, err := auth.NewTokenCipher(cfg.SessionEncryptionKey, cfg.OIDCClientSecret)
	if err != nil {
		return err
	}

	accountRules := make([]auth.AccountAccessRule, 0, len(cfg.Accounts))
	webAccounts := make([]web.AccountConfig, 0, len(cfg.Accounts))
	for _, acct := range cfg.Accounts {
		accountRules = append(accountRules, auth.AccountAccessRule{
			AccountID:   acct.ID,
			AccountName: acct.Name,
			OIDCGroup:   acct.OIDCGroup,
		})
		webAccounts = append(webAccounts, web.AccountConfig{ID: acct.ID, Name: acct.Name, FromAddress: acct.SMTPFromAddress})
	}
	authSvc := auth.NewService(queries, auditLogger, sessions, oidcProvider, cfg.BootstrapAdminMail, accountRules...).WithTokenCipher(tokenCipher)
	if _, _, err := authSvc.SeedBootstrapAdminAndSettings(ctx, cfg.BootstrapAdminMail, cfg.BootstrapAdminName, cfg.DisplayTimezone); err != nil {
		return err
	}

	assignEngine := assignment.NewEngine(0.5, assignment.NewDefaultAssigneeResolver(queries))
	matcher := threading.NewMatcher(queries, assignEngine, auditLogger)

	imapJobWakeCh := make(chan struct{}, 16)
	wfSvc := workflow.NewService(workflow.QueriesAdapter{Queries: queries}, auditLogger, imapJobWakeCh)

	// One IMAP sync worker per configured mail account.
	accountDialers := make(map[string]func(context.Context) (mail.IMAPClient, error), len(cfg.Accounts))
	accountEndpoints := make(map[string]mail.IMAPEndpoint, len(cfg.Accounts))
	accountSenders := make([]mail.AccountSenderConfig, 0, len(cfg.Accounts))
	syncWorkers := make([]*mail.IMAPSyncWorker, 0, len(cfg.Accounts))
	for _, acct := range cfg.Accounts {
		acct := acct
		dialer := func(_ context.Context) (mail.IMAPClient, error) {
			return mail.DialIMAP(acct.IMAPHost, acct.IMAPPort, acct.IMAPUsername, acct.IMAPPassword, acct.IMAPImplicitTLS)
		}
		accountDialers[acct.ID] = dialer
		endpoint := mail.IMAPEndpoint{Host: acct.IMAPHost, Port: acct.IMAPPort, Username: acct.IMAPUsername}
		accountEndpoints[acct.ID] = endpoint
		syncWorkers = append(syncWorkers,
			mail.NewIMAPSyncWorker(acct.ID, queries, matcher, attStore, dialer, acct.IMAPPollInterval, logger).WithEndpoint(endpoint).WithInitialSyncDays(acct.IMAPInitialSyncDays).
				WithTransitioner(wfSvc).WithOwnAddresses(acct.SMTPFromAddress, acct.IMAPUsername, acct.SMTPUsername))
		accountSenders = append(accountSenders, mail.AccountSenderConfig{
			AccountID:    acct.ID,
			FromAddress:  acct.SMTPFromAddress,
			FromName:     acct.SMTPFromName,
			Transport:    mail.NewGoMailTransport(acct.SMTPHost, acct.SMTPPort, acct.SMTPUsername, acct.SMTPPassword, acct.SMTPTLSMode),
			SMTPServer:   net.JoinHostPort(acct.SMTPHost, strconv.Itoa(acct.SMTPPort)),
			SMTPUsername: acct.SMTPUsername,
		})
	}

	accountDialer := func(ctx context.Context, accountID string) (mail.IMAPClient, error) {
		dialer, ok := accountDialers[accountID]
		if !ok {
			return nil, fmt.Errorf("no IMAP account configured for %q", accountID)
		}
		return dialer(ctx)
	}
	actionWorker := mail.NewIMAPActionWorker(queries, attStore, accountDialer, imapJobWakeCh, logger).WithEndpoints(accountEndpoints)

	outboxWorker := mail.NewOutboxWorker(
		mail.QueriesOutboxAdapter{Queries: queries},
		attStore,
		accountSenders,
		auditLogger,
		cfg.MailDomain,
		imapJobWakeCh,
		logger,
	)

	for _, worker := range syncWorkers {
		go worker.Run(ctx)
	}
	go actionWorker.Run(ctx)
	go outboxWorker.Run(ctx)

	webSrv := web.NewServer(queries, authSvc, wfSvc, outboxWorker, attStore, auditLogger, sessions, webAccounts).
		WithInboxPageSize(cfg.InboxPageSize).
		WithBranding(web.AppBranding{
			Name:        cfg.AppName,
			ShortName:   cfg.AppShortName,
			Description: cfg.AppDescription,
		})
	router := web.NewRouter(webSrv, authSvc, sessions)

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("minicrm_listening", slog.String("addr", cfg.HTTPAddr))
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
