package pages

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const usage = "Expected the prepare or verify-live subcommand"

// Run executes one delivery phase. prepare verifies, signs and assembles the
// site; verify-live reads the deployed site back.
func Run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet("queue-pages "+arguments[0], flag.ContinueOnError)
	tag := flags.String("expected-tag", "", "Requested public release tag")
	commit := flags.String("expected-commit", "", "Requested full source commit")
	artifact := flags.String("artifact-dir", "", "Directory of this run's notarized artifact")
	siteDir := flags.String("site-dir", "site", "Homepage files from the automation commit")
	outDir := flags.String("output-dir", "", "Directory for the assembled site")
	resultFile := flags.String("result-file", "", "File that receives deploy=true or deploy=false")
	publicKey := flags.String("public-key", "packaging/sparkle/public-ed-key.txt", "File holding the base64 Sparkle public key the app trusts")
	signTool := flags.String("sign-update", "", "Path to Sparkle sign_update")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("Unexpected delivery arguments")
	}
	d := newDelivery(context.Background(), output)
	switch arguments[0] {
	case "prepare":
		environment := map[string]string{}
		for _, name := range []string{"GITHUB_ACTIONS", "GITHUB_REPOSITORY", "GITHUB_REF"} {
			environment[name] = os.Getenv(name)
		}
		if !publicationAllowed(environment) {
			return errors.New("Delivery is restricted to the distribution workflow on main")
		}
		d.signTool, d.signKey = *signTool, strings.TrimSpace(os.Getenv(keyVariable))
		// The key travels on signer stdin only, so no child inherits it.
		_ = os.Unsetenv(keyVariable)
		if d.signKey == "" || d.signTool == "" {
			return errors.New(keyVariable + " and --sign-update are required to deliver. " + docsPointer)
		}
		key, err := loadPublicKey(*publicKey)
		if err != nil {
			return err
		}
		d.public = key
		return d.prepare(request{tagCommit{*tag, *commit}, *artifact, *siteDir, *outDir, *resultFile})
	case "verify-live":
		if *outDir == "" || *artifact == "" {
			return errors.New("verify-live requires --output-dir and --artifact-dir")
		}
		key, err := loadPublicKey(*publicKey)
		if err != nil {
			return err
		}
		d.public = key
		return d.verifyLive(*outDir, *artifact)
	}
	return errors.New(usage)
}

func Main(arguments []string) int {
	if err := Run(arguments, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
