package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/wenzwork/wenzwork-web/server/internal/remotepoc"
)

func main() {
	outcome, err := remotepoc.Run(context.Background(), time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "remote POC failed:", err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(outcome); err != nil {
		fmt.Fprintln(os.Stderr, "encode remote POC outcome:", err)
		os.Exit(1)
	}
}
