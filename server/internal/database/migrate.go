package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Migrate applies all forward PostgreSQL migrations. It is shared by the
// command-line migrator and the authenticated first-login setup flow.
func Migrate(ctx context.Context, databaseURL, migrationsDir string) error {
	databaseURL = strings.TrimSpace(databaseURL)
	migrationsDir = strings.TrimSpace(migrationsDir)
	if databaseURL == "" || migrationsDir == "" {
		return fmt.Errorf("database URL and migrations directory are required")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open migration database: %w", err)
	}
	defer db.Close()
	pingContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingContext); err != nil {
		return fmt.Errorf("ping migration database: %w", err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set migration dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, migrationsDir); err != nil {
		return fmt.Errorf("apply database migrations: %w", err)
	}
	return nil
}
