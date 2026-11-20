// Command gordon is Gordon's one binary. Its first argument picks the
// mode: receiver (the public webhook intake), worker (decisions, actions
// and reconciliation) or migrate (schema and role grants, as the schema
// owner). Configuration is environment variables; see DEPLOYMENT.md.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sperano/gordon/internal/app"
	"github.com/sperano/gordon/internal/config"
	"github.com/sperano/gordon/internal/logging"
)

// Exit codes.
const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

const usage = "usage: gordon receiver|worker|migrate"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, env config.Getenv, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	boot := logging.New(stdout, slog.LevelInfo).With("mode", args[0])
	var err error
	switch args[0] {
	case "receiver":
		var cfg config.Receiver
		if cfg, err = config.LoadReceiver(env); err == nil {
			err = app.Receiver(ctx, cfg, logger(stdout, cfg.LogLevel, args[0]), nil)
		}
	case "worker":
		var cfg config.Worker
		if cfg, err = config.LoadWorker(env); err == nil {
			err = app.Worker(ctx, cfg, logger(stdout, cfg.LogLevel, args[0]), nil)
		}
	case "migrate":
		var cfg config.Migrate
		if cfg, err = config.LoadMigrate(env); err == nil {
			err = app.Migrate(ctx, cfg, logger(stdout, cfg.LogLevel, args[0]))
		}
	default:
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	if err != nil {
		boot.Error("exiting", "error", err)
		return exitFailed
	}
	return exitOK
}

func logger(w io.Writer, level slog.Level, mode string) *slog.Logger {
	return logging.New(w, level).With("mode", mode)
}
