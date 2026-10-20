// Command launch runs the launch windows on the production relay server.
// See product/issues/catalog-rotation-script.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `Usage:
  launch preflight                                 Ask for the services and paths, and check them
  launch deploy-v2                                 Window 1: relay v2 on the current catalog key
  launch rotate                                    Window 2: the new catalog key
  launch rotate --dry-run --relay <binary> <backup>  Rotate a copy of a backup with a throwaway key
  launch restore <backup>                          Put a backup back

Answers are kept in launch.json in the working directory. Run as root on the
production server. --dry-run runs anywhere and touches only a temporary copy.
`

var errUsage = errors.New("see usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage)
		}
		fmt.Fprintf(os.Stderr, "launch: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	t := newTerm(os.Stdin, os.Stdout)
	switch args[0] {
	case "preflight":
		_, err := loadSite(ctx, t)
		return err
	case "deploy-v2":
		return deployV2(ctx, t)
	case "rotate":
		fs := flag.NewFlagSet("rotate", flag.ContinueOnError)
		dry := fs.Bool("dry-run", false, "rotate a copy of a backup")
		relayBin := fs.String("relay", "", "relay v2 binary for --dry-run")
		if err := fs.Parse(args[1:]); err != nil {
			return errUsage
		}
		if !*dry {
			if fs.NArg() != 0 {
				return errUsage
			}
			return rotate(ctx, t)
		}
		if fs.NArg() != 1 || *relayBin == "" {
			return errUsage
		}
		return dryRun(ctx, t, fs.Arg(0), *relayBin)
	case "restore":
		if len(args) != 2 {
			return errUsage
		}
		return restore(ctx, t, args[1])
	default:
		return errUsage
	}
}
