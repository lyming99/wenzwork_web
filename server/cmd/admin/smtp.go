package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wenzwork/wenzwork-web/server/internal/config"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
)

func runSMTPTest(args []string) int {
	envFile := ""
	switch {
	case len(args) == 0:
		config.LoadDevelopmentEnv()
	case len(args) == 2 && args[0] == "--env-file" && strings.TrimSpace(args[1]) != "":
		envFile = args[1]
		if err := config.LoadEnvFile(envFile); err != nil {
			fmt.Fprintf(os.Stderr, "load SMTP test configuration: %v\n", err)
			return 1
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: wenzwork-admin smtp test [--env-file PATH]")
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		return 1
	}
	recipient := strings.TrimSpace(os.Getenv("SYSTEM_ADMIN_EMAIL"))
	if recipient == "" {
		fmt.Fprintln(os.Stderr, "SYSTEM_ADMIN_EMAIL is required")
		return 2
	}

	sender, err := mailer.NewSMTPSender(mailer.SMTPConfig{
		Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUser, Password: cfg.SMTPPassword,
		From: cfg.MailFrom, RequireTLS: cfg.Environment == "production", Timeout: 10 * time.Second,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create SMTP sender: %v\n", err)
		return 1
	}

	tlsMode := "optional STARTTLS"
	if cfg.Environment == "production" {
		tlsMode = "required STARTTLS"
	}
	fmt.Printf("testing SMTP delivery via %s:%d to %s (%s)\n", cfg.SMTPHost, cfg.SMTPPort, recipient, tlsMode)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sender.Send(ctx, mailer.Message{
		To:      recipient,
		Subject: "WenzWork SMTP initialization check",
		Text:    fmt.Sprintf("SMTP configuration verified by wenzwork-admin at %s.\n", time.Now().UTC().Format(time.RFC3339)),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "send SMTP test message: %v\n", err)
		return 1
	}
	fmt.Printf("SMTP test message accepted for %s\n", recipient)
	return 0
}
