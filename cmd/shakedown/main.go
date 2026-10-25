package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/saikiransangam/shakedown/internal/cli"
)

var version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, cli.Usage)
		os.Exit(2)
	}

	// Ctrl-C cancels the context rather than killing the process, so a run in
	// progress still prints what it measured.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		code int
		err  error
	)
	switch os.Args[1] {
	case "run":
		code, err = cli.Run(ctx, os.Args[2:])
	case "ramp":
		code, err = cli.Ramp(ctx, os.Args[2:])
	case "mock":
		code, err = cli.Mock(ctx, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("shakedown", version)
	case "help", "--help", "-h":
		fmt.Print(cli.Usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], cli.Usage)
		os.Exit(2)
	}

	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "shakedown:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
