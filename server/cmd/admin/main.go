package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wenzwork/wenzwork-web/server/internal/admincli"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/config"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
)

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "bootstrap":
			switch {
			case len(os.Args) == 2:
				os.Exit(runBootstrap())
			case len(os.Args) == 3 && os.Args[2] == "status":
				os.Exit(runBootstrapStatus())
			case len(os.Args) >= 3 && os.Args[2] == "relay-access-key":
				os.Exit(runBootstrapRelayAccessKey(os.Args[3:]))
			default:
				fmt.Fprintln(os.Stderr, "usage: wenzwork-admin bootstrap [status|relay-access-key]")
				os.Exit(2)
			}
		case "smtp":
			if len(os.Args) >= 3 && os.Args[2] == "test" {
				os.Exit(runSMTPTest(os.Args[3:]))
			}
			fmt.Fprintln(os.Stderr, "usage: wenzwork-admin smtp test [--env-file PATH]")
			os.Exit(2)
		case "s3":
			if len(os.Args) >= 3 && os.Args[2] == "test" {
				os.Exit(runS3Test(os.Args[3:]))
			}
			fmt.Fprintln(os.Stderr, "usage: wenzwork-admin s3 test [--env-file PATH]")
			os.Exit(2)
		}
	}
	if err := admincli.Run(context.Background(), os.Args[1:], os.Stdout, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if admincli.IsUsageError(err) {
			os.Exit(2)
		}
		os.Exit(1)
	}
	return
}

func runBootstrapStatus() int {
	config.LoadDevelopmentEnv()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		return 1
	}
	db, err := database.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		return 1
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	email, initialized, err := auth.SuperAdminEmail(context.Background(), db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check bootstrap status: %v\n", err)
		return 1
	}
	if !initialized {
		fmt.Println("uninitialized")
		return 0
	}
	fmt.Printf("initialized\t%s\n", email)
	return 0
}

func runBootstrap() int {
	config.LoadDevelopmentEnv()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		return 1
	}
	email := strings.TrimSpace(os.Getenv("BOOTSTRAP_ADMIN_EMAIL"))
	password := os.Getenv("BOOTSTRAP_ADMIN_PASSWORD")
	displayName := strings.TrimSpace(os.Getenv("BOOTSTRAP_ADMIN_DISPLAY_NAME"))
	if displayName == "" {
		displayName = "WenzWork 管理员"
	}
	if email == "" || password == "" {
		fmt.Fprintln(os.Stderr, "BOOTSTRAP_ADMIN_EMAIL and BOOTSTRAP_ADMIN_PASSWORD are required")
		return 2
	}

	db, err := database.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		return 1
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	result, err := auth.BootstrapSuperAdmin(context.Background(), db, email, password, displayName, auth.Argon2Params{
		MemoryKiB: cfg.Argon2MemoryKiB, Iterations: cfg.Argon2Iterations, Parallelism: cfg.Argon2Parallelism,
		SaltLength: 16, KeyLength: 32,
	})
	if err != nil {
		if errors.Is(err, auth.ErrBootstrapAlreadyComplete) {
			fmt.Fprintln(os.Stderr, "bootstrap refused: a different super administrator already exists")
		} else {
			fmt.Fprintf(os.Stderr, "bootstrap failed: %v\n", err)
		}
		return 1
	}
	if result.Created {
		fmt.Printf("created super administrator %s; sign in and enroll TOTP before using admin APIs\n", result.User.ID)
		return 0
	}
	fmt.Printf("super administrator %s already exists; no changes made\n", result.User.ID)
	return 0
}
