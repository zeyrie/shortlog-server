package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"

	"shortlog-server/db"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// migrate applies pending migrations. A Postgres advisory lock keeps concurrent
// deploys from running the same migration twice.
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	results, err := provider.Up(ctx)
	for _, result := range results {
		slog.Info("migration applied", "version", result.Source.Version, "file", result.Source.Path, "duration", result.Duration)
	}
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	slog.Info("migrations complete", "applied", len(results))
	return nil
}
