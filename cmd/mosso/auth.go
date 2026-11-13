package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/riggleg/mosso/internal/claude"
)

const authUsage = "usage: mosso auth claude"

// runAuth implements `mosso auth claude`: generate a Claude Code token with
// the official `claude setup-token` flow and store it in the macOS Keychain.
// Mosso never sees the token on its command line or in its output.
func runAuth(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "claude" {
		return errors.New(authUsage)
	}
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
