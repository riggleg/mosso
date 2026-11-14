package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/keychain"
)

const authUsage = "usage: mosso auth claude|github"

// runAuth implements `mosso auth claude` and `mosso auth github`.
func runAuth(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New(authUsage)
	}
	switch args[0] {
	case "claude":
		return authClaude(ctx)
	case "github":
		return authGitHub(ctx)
	}
	return errors.New(authUsage)
}

// authClaude generates a Claude Code token with the official
// `claude setup-token` flow and stores it in the macOS Keychain. Mosso never
// sees the token on its command line or in its output.
func authClaude(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("the macOS Keychain is not available on %s: run `claude setup-token` and export %s in the environment that starts Mosso", runtime.GOOS, claude.EnvToken)
	}

	fmt.Println("Step 1/2: generate a long-lived token for your Claude subscription with `claude setup-token`.")
	if path, err := exec.LookPath("claude"); err != nil {
		fmt.Println("Claude Code is not installed on this Mac. Run `claude setup-token` wherever it is installed, then continue.")
	} else {
		cmd := exec.CommandContext(ctx, path, "setup-token")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("claude setup-token: %w", err)
		}
	}

	fmt.Println()
	fmt.Printf("Step 2/2: paste the token at both prompts below (input is hidden). It is saved in your login Keychain as service %q, account %q.\n",
		claude.KeychainService, claude.KeychainAccount)
	if err := claude.StoreInKeychain(ctx, os.Stdin, os.Stdout, os.Stderr); err != nil {
		return err
	}
	if _, _, err := claude.LoadToken(ctx); err != nil {
		return fmt.Errorf("the stored token is not usable: %w", err)
	}
	fmt.Println("Token stored. Running Mosso servers pick it up on the next message; no restart needed.")
	fmt.Println("The token is still visible in this terminal's scrollback: clear it (Cmd-K in Terminal/iTerm).")
	return nil
}

// authGitHub stores a GitHub personal access token in the macOS Keychain
// and checks that it works.
func authGitHub(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("the macOS Keychain is not available on %s: export %s in the environment that starts Mosso", runtime.GOOS, github.EnvToken)
	}
	fmt.Println("Step 1/2: create a fine-grained personal access token. This link opens GitHub's form with")
	fmt.Println("Mosso's permissions preselected (Contents and Pull requests: Read and write; Metadata: Read)")
	fmt.Println("and a 90-day expiration:")
	fmt.Println("  " + github.NewTokenURL)
	fmt.Println("Under Repository access, choose only the repositories Mosso should work on, then generate the token.")
	fmt.Println("(You can also paste the token into Connect GitHub in the Mosso web UI instead.)")
	fmt.Println()
	fmt.Printf("Step 2/2: paste the token at both prompts below (input is hidden). It is saved in your login Keychain as service %q, account %q.\n",
		keychain.Service, github.KeychainAccount)
	if err := github.StoreInKeychain(ctx, os.Stdin, os.Stdout, os.Stderr); err != nil {
		return err
	}
	tok, _, err := github.LoadToken(ctx)
	if err != nil {
		return fmt.Errorf("the stored token is not usable: %w", err)
	}
	user, err := github.NewClient(tok).User(ctx)
	if err != nil {
		return fmt.Errorf("the token was stored but GitHub rejected it: %w", err)
	}
	fmt.Printf("Token stored for GitHub user %s. Running Mosso servers pick it up on the next request; no restart needed.\n", user.Login)
	return nil
}
