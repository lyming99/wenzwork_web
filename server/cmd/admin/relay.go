package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/config"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

func runBootstrapRelayAccessKey(arguments []string) int {
	flags := flag.NewFlagSet("bootstrap relay-access-key", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	platform := flags.String("platform", relayBootstrapPlatform(), "Relay target platform")
	architecture := flags.String("architecture", relayBootstrapArchitecture(), "Relay target architecture")
	publicEndpoint := flags.String("public-endpoint", "", "client-facing ws:// or wss:// Relay endpoint ending in /v2/connect")
	listenerPort := flags.Int("listener-port", 8443, "plaintext Relay WebSocket listener port")
	displayName := flags.String("display-name", "One-click Relay", "Relay installation display name")
	output := flags.String("output", "", "owner-only file that receives the one-time Access Key")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: wenzwork-admin bootstrap relay-access-key [--platform linux] [--architecture amd64] [--listener-port 8443] [--public-endpoint wss://relay.example.com/v2/connect] [--output PATH]")
		return 2
	}
	if path := strings.TrimSpace(*output); path != "" {
		if _, err := os.Lstat(path); err == nil {
			fmt.Fprintln(os.Stderr, "output file already exists; refusing to replace a possible Relay Access Key")
			return 1
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "inspect Relay Access Key output: %v\n", err)
			return 1
		}
	}

	config.LoadDevelopmentEnv()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		return 1
	}
	ctx := context.Background()
	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		return 1
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	actorID, initialized, err := auth.SuperAdminID(ctx, db)
	if err != nil || !initialized {
		if err == nil {
			err = fmt.Errorf("the first super administrator has not been initialized")
		}
		fmt.Fprintf(os.Stderr, "resolve bootstrap administrator: %v\n", err)
		return 1
	}
	store, err := relaymanagement.NewStore(db, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create Relay management store: %v\n", err)
		return 1
	}
	installation, err := store.CreateInstallation(ctx, relaymanagement.CreateInstallationInput{
		DisplayName: strings.TrimSpace(*displayName), Region: cfg.RemoteMVPRegion,
		Group: cfg.RemoteMVPPool, FailureDomain: "one-click", PublicEndpoint: strings.TrimSpace(*publicEndpoint),
		ListenerPort: *listenerPort,
		Platform:     strings.ToLower(strings.TrimSpace(*platform)), Architecture: strings.ToLower(strings.TrimSpace(*architecture)),
		ActorUserID: actorID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create Relay installation: %v\n", err)
		return 1
	}
	accessKey, err := store.CreateAccessKey(ctx, installation.ID, actorID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create Relay Access Key: %v\n", err)
		return 1
	}
	if strings.TrimSpace(*output) == "" {
		fmt.Println(accessKey.Key)
		return 0
	}
	if err := writeBootstrapSecret(*output, accessKey.Key+"\n"); err != nil {
		fmt.Fprintf(os.Stderr, "write Relay Access Key: %v\n", err)
		return 1
	}
	return 0
}

func writeBootstrapSecret(path, value string) error {
	path = strings.TrimSpace(path)
	if path == "" || strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("output path is invalid")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.WriteString(value); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func relayBootstrapPlatform() string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		return runtime.GOOS
	}
	return "linux"
}

func relayBootstrapArchitecture() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}
