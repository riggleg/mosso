// Package keychain reads and stores Mosso's secrets as generic passwords in
// the macOS login Keychain through /usr/bin/security. Secret values travel
// over stdin/stdout only, never command arguments.
package keychain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
)

// Service is the Keychain service name for every Mosso item; the account
// name tells them apart.
const Service = "mosso"

const (
	securityTool = "/usr/bin/security"
	// errSecItemNotFound is the exit status of `security` for a missing item.
	errSecItemNotFound = 44
)

// Available reports whether the macOS Keychain can be used on this system.
func Available() bool { return runtime.GOOS == "darwin" }

// Read returns the secret stored for account, or "" if there is none or the
// Keychain is not available on this system.
func Read(ctx context.Context, account string) (string, error) {
	if !Available() {
		return "", nil
	}
	// The secret arrives on stdout; it is never passed as an argument.
	cmd := exec.CommandContext(ctx, securityTool, "find-generic-password", "-s", Service, "-a", account, "-w")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == errSecItemNotFound {
			return "", nil
		}
		return "", fmt.Errorf("read macOS Keychain: %w", err)
	}
	return strings.TrimSpace(out.String()), nil
}

// Write saves secret for account in the login Keychain, replacing any
// existing item, without prompting. It is for secrets pasted into Mosso's
// web UI. The secret reaches `security` on stdin, as a command for its
// interactive mode (-i), so it never appears in process arguments. secret
// and label must not contain quotes, backslashes or line breaks; secret must
// not contain spaces.
func Write(ctx context.Context, account, label, secret string) error {
	if !Available() {
		return errors.New("the macOS Keychain is only available on macOS")
	}
	if secret == "" || strings.ContainsAny(secret, " \t\r\n\"'\\") || strings.ContainsAny(account+label, "\r\n\"'\\") {
		return errors.New("keychain: unsupported characters in the secret or its label")
	}
	cmd := exec.CommandContext(ctx, securityTool, "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -l \"%s\" -w %s\n", Service, account, label, secret))
	// Interactive mode may echo the command, secret included: discard it.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("store secret in macOS Keychain: %w", err)
	}
	// `security -i` exits 0 even when its command fails, so read it back.
	got, err := Read(ctx, account)
	if err != nil {
		return err
	}
	if got != secret {
		return errors.New("store secret in macOS Keychain: the item was not saved (was Keychain access denied?)")
	}
	return nil
}

// Store saves a secret for account in the login Keychain, replacing any
// existing one. `security` prompts for the value on the terminal without
// echoing it, so the secret never appears in arguments or shell history.
func Store(ctx context.Context, account, label string, stdin io.Reader, stdout, stderr io.Writer) error {
	if !Available() {
		return errors.New("the macOS Keychain is only available on macOS")
	}
	cmd := exec.CommandContext(ctx, securityTool, "add-generic-password", "-U",
		"-s", Service, "-a", account, "-l", label,
		"-w") // -w last: prompt for the password
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("store secret in macOS Keychain: %w", err)
	}
	return nil
}
