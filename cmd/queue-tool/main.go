package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Dunky13/agent-job-queue/internal/notaryapi"
	"github.com/Dunky13/agent-job-queue/internal/tooling"
)

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("Expected check-source, check-commits, render-cask, verify-release, or notarize")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "verify-checkpoint-origin":
		original := flags.String("original", "", "verified original installer ZIP")
		initial := flags.String("initial", "", "checkpoint initial installer ZIP")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return tooling.VerifyCheckpointOrigin(*original, *initial)
	case "verify-build-artifact":
		archive := flags.String("archive", "", "original GitHub artifact ZIP")
		digest := flags.String("digest", "", "GitHub artifact SHA-256 digest")
		commit := flags.String("commit", "", "original source commit")
		build := flags.Int("build", 0, "original build number")
		output := flags.String("output", "", "new verified artifact directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		version, err := tooling.VerifyBuildArtifact(*archive, *digest, *commit, *build, *output)
		if err == nil {
			fmt.Println(version)
		}
		return err
	case "notarize":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return notaryapi.Run(ctx, args[1:], os.Stdout)
	case "shift-fixture":
		input := flags.String("input", "", "public fixture input")
		output := flags.String("output", "", "rebased output")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return tooling.ShiftFixture(*input, *output)
	case "verify-installer":
		installer := flags.String("installer", "", "extracted installer app")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return tooling.VerifyInstaller(*installer)
	case "licenses":
		output := flags.String("output", "", "license output directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *output == "" {
			return errors.New("Output directory is required")
		}
		return tooling.WriteLicenses(*output)
	case "check-source":
		root := flags.String("root", ".", "repository root")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return tooling.CheckSource(*root)
	case "check-commits":
		base := flags.String("base", "", "base revision")
		head := flags.String("head", "HEAD", "head revision")
		local := flags.Bool("verify-local", false, "verify Git signatures")
		repo := flags.String("github-repository", "", "GitHub signature verification repository")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return tooling.CheckCommits(*base, *head, *local, *repo)
	case "render-cask":
		archive := flags.String("archive", "", "installer ZIP")
		version := flags.String("version", "", "version")
		tag := flags.String("tag", "", "immutable tag")
		commit := flags.String("commit", "", "commit")
		build := flags.Int("build", 0, "build number")
		output := flags.String("output", "", "output directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *output == "" {
			return errors.New("Output directory is required")
		}
		return tooling.RenderRelease(*archive, *version, *tag, *commit, *build, *output)
	case "verify-release":
		existing := flags.String("existing", "", "downloaded release directory")
		assets := flags.String("assets", "dist", "local assets directory")
		commit := flags.String("commit", "", "expected target commit")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		return tooling.VerifyRelease(*existing, *assets, *commit)
	}
	return fmt.Errorf("Unknown tool %q", args[0])
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
