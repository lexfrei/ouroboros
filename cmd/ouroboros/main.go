// Package main is the entry point for the ouroboros binary, dispatching to
// the controller or proxy subcommand.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cockroachdb/errors"
)

var (
	version  = "dev"
	revision = "unknown"
)

func main() {
	exitCode, err := run(os.Args[1:])
	if err != nil {
		slog.Default().Error("fatal", "error", err)
	}

	os.Exit(exitCode)
}

func run(args []string) (int, error) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if len(args) == 0 {
		printUsage()

		return 2, nil
	}

	sub := args[0]
	rest := args[1:]

	ctx, cancel := signalContext()
	defer cancel()

	switch sub {
	case "controller":
		err := runController(ctx, logger, rest)
		if err != nil {
			return 1, err
		}

		return 0, nil
	case "proxy":
		err := runProxy(ctx, logger, rest)
		if err != nil {
			return 1, err
		}

		return 0, nil
	case "version", "-v", "--version":
		_, _ = fmt.Fprintf(os.Stdout, "ouroboros %s (%s)\n", version, revision)

		return 0, nil
	case "help", "-h", "--help":
		printUsage()

		return 0, nil
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", sub)

		printUsage()

		return 2, errors.Errorf("unknown subcommand %q", sub)
	}
}

func printUsage() {
	_, _ = fmt.Fprintln(os.Stderr, `ouroboros — DNS rewriting + PROXY-protocol injection for in-cluster ingress traffic

Usage:
  ouroboros controller [flags]   watch Ingress/Gateway and update CoreDNS or /etc/hosts
  ouroboros proxy      [flags]   run the TCP relay that injects PROXY-protocol v1 headers
  ouroboros version              print version info

Run 'ouroboros <subcommand> --help' for flag details.`)
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-signals
		cancel()
	}()

	return ctx, cancel
}
