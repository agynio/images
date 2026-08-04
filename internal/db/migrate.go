package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/agynio/images/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	return pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
			return fmt.Errorf("ensure schema_migrations: %w", err)
		}

		entries, err := fs.ReadDir(migrations.Files, ".")
		if err != nil {
			return fmt.Errorf("read migrations: %w", err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			version := entry.Name()
			var applied bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
				return fmt.Errorf("check migration %s: %w", version, err)
			}
			if applied {
				continue
			}
			content, err := migrations.Files.ReadFile(version)
			if err != nil {
				return fmt.Errorf("read migration %s: %w", version, err)
			}
			if _, err := tx.Exec(ctx, string(content)); err != nil {
				return fmt.Errorf("apply migration %s: %w", version, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
				return fmt.Errorf("record migration %s: %w", version, err)
			}
		}
		return nil
	})
}
