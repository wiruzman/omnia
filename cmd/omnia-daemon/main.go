package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"omnia-search-tui/internal/daemon"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := daemon.RunMain(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "daemon error: %v\n", err)
		os.Exit(1)
	}
}
