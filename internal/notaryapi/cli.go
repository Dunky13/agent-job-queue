package notaryapi

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"time"
)

// Run executes the environment-authenticated notarization CLI. Endpoint and
// transport injection are intentionally private to this package's tests.
func Run(ctx context.Context, args []string, stdout io.Writer) error {
	return run(ctx, args, stdout, os.Getenv, newClient)
}

func run(ctx context.Context, args []string, stdout io.Writer, getenv func(string) string, factory func(*credentials) *client) error {
	if len(args) == 0 {
		return failure("invalid_arguments")
	}
	flags := flag.NewFlagSet("notarize", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var archive, record, hash, id, output string
	var timeout time.Duration
	switch args[0] {
	case "preflight":
	case "submit":
		flags.StringVar(&archive, "archive", "", "archive to upload")
		flags.StringVar(&record, "record", "", "durable stage record")
		flags.StringVar(&hash, "sha256", "", "prepared archive SHA-256")
	case "wait":
		flags.StringVar(&id, "id", "", "existing Apple submission UUID")
		flags.DurationVar(&timeout, "timeout", 20*time.Minute, "maximum wait")
	case "log":
		flags.StringVar(&id, "id", "", "existing Apple submission UUID")
		flags.StringVar(&output, "output", "", "private JSON log output")
	default:
		return failure("invalid_arguments")
	}
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return failure("invalid_arguments")
	}
	if (args[0] == "wait" || args[0] == "log") && !uuidPattern.MatchString(id) {
		return failure("invalid_arguments")
	}
	creds, err := loadCredentials(getenv)
	var c *client
	if err == nil {
		c = factory(creds)
		if transport, ok := c.transport.(interface{ CloseIdleConnections() }); ok {
			defer transport.CloseIdleConnections()
		}
	}
	if args[0] == "preflight" {
		if err == nil {
			err = c.preflight(ctx)
		}
		if err != nil {
			if writeErr := json.NewEncoder(stdout).Encode(struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}{"error", stringError(err)}); writeErr != nil {
				return failure("output_failed")
			}
			return err
		}
		if json.NewEncoder(stdout).Encode(struct {
			Status string `json:"status"`
		}{"ready"}) != nil {
			return failure("output_failed")
		}
		return nil
	}
	if err != nil {
		return err
	}
	var r result
	switch args[0] {
	case "submit":
		r, err = c.submit(ctx, archive, record, hash, getenv("GITHUB_RUN_ATTEMPT"), getenv("GITHUB_RUN_ID"))
	case "wait":
		r, err = c.wait(ctx, id, timeout)
	case "log":
		return c.log(ctx, id, output)
	}
	// A known provider ID is public recovery evidence even when persistence or S3
	// fails. Callers must retain stdout on nonzero exit; errors remain fixed codes.
	if uuidPattern.MatchString(r.ID) {
		if json.NewEncoder(stdout).Encode(r) != nil {
			return failure("output_failed")
		}
	}
	return err
}

func stringError(err error) string {
	if code, ok := err.(failure); ok {
		return string(code)
	}
	return "request_failed"
}
