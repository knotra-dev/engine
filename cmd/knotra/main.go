package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/knotra-dev/engine/internal/cli"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cli.Execute(ctx, os.Args[1:], cli.Options{Version: version})
	cancel()
	os.Exit(cli.ExitCode(err))
}
