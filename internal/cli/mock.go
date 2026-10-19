package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/saikiransangam/shakedown/mockapi"
)

// Mock executes the `shakedown mock` subcommand.
func Mock(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("mock", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: shakedown mock [flags]\n\nProfiles:\n%s\n", mockapi.Describe())
		fs.PrintDefaults()
	}

	addr := fs.String("listen", "127.0.0.1:9090", "address to listen on")
	profileName := fs.String("profile", "flaky", "fault profile to serve")
	list := fs.Bool("list-profiles", false, "list available profiles and exit")

	if err := fs.Parse(args); err != nil {
		return 2, nil
	}
	if *list {
		fmt.Print(mockapi.Describe())
		return 0, nil
	}

	profile, err := mockapi.LookupProfile(*profileName)
	if err != nil {
		return 2, err
	}

	server := mockapi.New(profile)
	fmt.Fprintf(os.Stderr, "mock api on http://%s  profile=%s\n  %s\n", *addr, profile.Name, profile.Description)
	fmt.Fprintf(os.Stderr, "  /healthz always answers 200 — just like a real service hiding a brownout\n")

	if err := server.Serve(ctx, *addr); err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stderr, "\nserved %d requests\n", server.Served())
	return 0, nil
}
