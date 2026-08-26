package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/wenzwork/wenzwork-web/server/internal/config"
)

func main() {
	config.LoadDevelopmentEnv()
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(2)
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		fmt.Fprintf(os.Stderr, "set migration dialect: %v\n", err)
		os.Exit(1)
	}

	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	arguments := []string(nil)
	if len(os.Args) > 2 {
		arguments = os.Args[2:]
	}
	if err := validateCommand(command, arguments, os.Getenv("ALLOW_DESTRUCTIVE_MIGRATIONS")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/migrate [up|status|down-to <version>]")
		os.Exit(2)
	}

	if err := goose.Run(command, db, "migrations", arguments...); err != nil {
		fmt.Fprintf(os.Stderr, "migration %s failed: %v\n", command, err)
		os.Exit(1)
	}
}

func validateCommand(command string, arguments []string, destructiveOptIn string) error {
	switch command {
	case "up", "status":
		if len(arguments) != 0 {
			return errors.New("migration command does not accept arguments")
		}
		return nil
	case "down-to":
		if destructiveOptIn != "1" {
			return errors.New("down-to requires ALLOW_DESTRUCTIVE_MIGRATIONS=1")
		}
		if len(arguments) != 1 {
			return errors.New("down-to requires one target version")
		}
		version, err := strconv.ParseUint(arguments[0], 10, 63)
		if err != nil || version == 0 {
			return errors.New("down-to target version is invalid")
		}
		return nil
	default:
		return errors.New("unsupported migration command")
	}
}
