package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wenzwork/wenzwork-web/server/internal/config"
	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
)

func runS3Test(args []string) int {
	switch {
	case len(args) == 0:
		config.LoadDevelopmentEnv()
	case len(args) == 2 && args[0] == "--env-file" && strings.TrimSpace(args[1]) != "":
		if err := config.LoadEnvFile(args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "load S3 test configuration: %v\n", err)
			return 1
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: wenzwork-admin s3 test [--env-file PATH]")
		return 2
	}

	cfg := objectstore.S3Config{
		Endpoint:        os.Getenv("S3_ENDPOINT"),
		Region:          os.Getenv("S3_REGION"),
		Bucket:          os.Getenv("S3_BUCKET"),
		AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("S3_SESSION_TOKEN"),
		AddressingStyle: os.Getenv("S3_ADDRESSING_STYLE"),
	}

	addressingStyle, err := objectstore.ResolveS3AddressingStyle(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid S3 test configuration: %v\n", err)
		return 1
	}
	fmt.Printf("testing S3 write/read/delete for bucket %s (addressing: %s)\n", cfg.Bucket, addressingStyle)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := objectstore.CheckS3(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "S3 test failed: %v\n", err)
		return 1
	}
	fmt.Printf("S3 write, read, and delete checks succeeded for bucket %s\n", cfg.Bucket)
	return 0
}
