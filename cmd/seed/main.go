// Command seed loads declarative YAML demo content onto a running MiniCRM stack.
//
// It never migrates. Start minicrm first so the schema exists, then run seed with the same
// database and account environment variables the application uses.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wichert/minicrm/internal/config"
	"github.com/wichert/minicrm/internal/db/sqlc"
	"github.com/wichert/minicrm/internal/seed"
	"github.com/wichert/minicrm/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	configPath := flag.String("config", "", "path to the seed YAML document")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall seed timeout, including IMAP job drain")
	flag.Parse()
	if *configPath == "" {
		logger.Error("seed_fatal", slog.String("error", "-config is required"))
		os.Exit(2)
	}

	if err := runSeed(logger, *configPath, *timeout); err != nil {
		logger.Error("seed_fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func runSeed(logger *slog.Logger, configPath string, timeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return run(ctx, logger, configPath)
}

func run(ctx context.Context, logger *slog.Logger, configPath string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	doc, err := seed.LoadFile(configPath, "", cfg.Accounts)
	if err != nil {
		return err
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.ConnConfig.Tracer = sqlc.NewStructuredQueryTracer(logger)
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	attStore, err := storage.NewStore(cfg.AttachmentsDir, logger)
	if err != nil {
		return err
	}

	_, err = seed.Run(ctx, seed.Options{
		File:        doc,
		Accounts:    cfg.Accounts,
		Queries:     sqlc.New(pool),
		Pool:        pool,
		Attachments: attStore,
		Logger:      logger,
		Dial:        seed.DefaultDialer,
	})
	return err
}
