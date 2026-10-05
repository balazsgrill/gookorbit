package db

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	mpostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies embedded migrations to the database.
func Migrate(databaseURL string) error {
	src, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("migrate source: %w", err)
	}
	sqlDB, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return fmt.Errorf("open sql db: %w", err)
	}
	defer sqlDB.Close()
	drv, err := mpostgres.WithInstance(sqlDB, &mpostgres.Config{})
	if err != nil {
		return fmt.Errorf("migrate postgres driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		return fmt.Errorf("migrate instance: %w", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
