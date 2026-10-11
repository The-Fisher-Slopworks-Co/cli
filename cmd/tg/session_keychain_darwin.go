//go:build darwin

package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"

	"github.com/go-faster/errors"

	"github.com/gotd/td/session"
)

// keychainNotFound is the exit code `security` returns when an item is absent
// (errSecItemNotFound).
const keychainNotFound = 44

// keychainSessionStore returns a Keychain-backed store. The bool is always true
// on darwin; the signature mirrors the non-darwin stub so newSessionStore can
// fall back to a file when Keychain is unavailable. legacy is the pre-Keychain
// file session path, migrated into the Keychain (and removed) on first use.
func keychainSessionStore(service, account, legacy string) (sessionStore, bool) {
	return &keychainStore{service: service, account: account, legacy: legacy}, true
}

// keychainStore stores a gotd string session as a generic password in the macOS
// login Keychain, driven through the `security` CLI (no cgo, keeps the binary
// static).
type keychainStore struct {
	service string
	account string
	legacy  string // pre-Keychain file session, migrated then deleted.
}

// `security` flags that select a generic password by service and account.
const (
	keychainServiceFlag = "-s"
	keychainAccountFlag = "-a"
)

// keychainItemArgs selects the generic password by service and account.
func keychainItemArgs(service, account string) []string {
	return []string{keychainServiceFlag, service, keychainAccountFlag, account}
}

func keychainAddArgs(service, account, secret string) []string {
	// -U updates the item in place when it already exists.
	args := append([]string{"add-generic-password", "-U"}, keychainItemArgs(service, account)...)
	return append(args, "-w", secret)
}

func keychainFindArgs(service, account string) []string {
	// -w prints only the password to stdout.
	args := append([]string{"find-generic-password"}, keychainItemArgs(service, account)...)
	return append(args, "-w")
}

func keychainDeleteArgs(service, account string) []string {
	return append([]string{"delete-generic-password"}, keychainItemArgs(service, account)...)
}

// securityCmd runs the macOS `security` tool. The binary is fixed and the
// arguments are passed straight to exec (no shell), built by the
// keychain*Args helpers above from the configured service and account.
func securityCmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "security", args...) //nolint:gosec // G204: fixed binary, no shell; see above.
}

// isNotFound reports whether err is `security` signaling a missing item.
func isNotFound(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == keychainNotFound
}

func (s *keychainStore) LoadSession(ctx context.Context) ([]byte, error) {
	out, err := securityCmd(ctx, keychainFindArgs(s.service, s.account)...).Output()
	switch {
	case err == nil:
		// security appends a trailing newline to the password it prints.
		return bytes.TrimSuffix(out, []byte("\n")), nil
	case !isNotFound(err):
		return nil, errors.Wrap(err, "keychain find")
	}
	// Nothing in the Keychain: migrate a pre-Keychain file session if present.
	data, rerr := os.ReadFile(s.legacy)
	if errors.Is(rerr, os.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	if rerr != nil {
		return nil, errors.Wrap(rerr, "read legacy session")
	}
	if err := s.StoreSession(ctx, data); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *keychainStore) StoreSession(ctx context.Context, data []byte) error {
	if err := securityCmd(ctx, keychainAddArgs(s.service, s.account, string(data))...).Run(); err != nil {
		return errors.Wrap(err, "keychain add")
	}
	// Any write supersedes a pre-Keychain file session; remove it best-effort.
	if err := os.Remove(s.legacy); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "remove legacy session")
	}
	return nil
}

func (s *keychainStore) Exists(ctx context.Context) (bool, error) {
	err := securityCmd(ctx, keychainFindArgs(s.service, s.account)...).Run()
	if err == nil {
		return true, nil
	}
	if !isNotFound(err) {
		return false, errors.Wrap(err, "keychain find")
	}
	// Not yet migrated: a leftover file session still counts.
	if _, serr := os.Stat(s.legacy); serr == nil {
		return true, nil
	}
	return false, nil
}

func (s *keychainStore) Delete(ctx context.Context) error {
	err := securityCmd(ctx, keychainDeleteArgs(s.service, s.account)...).Run()
	if err != nil && !isNotFound(err) {
		return errors.Wrap(err, "keychain delete")
	}
	// Also drop any pre-Keychain file session.
	if rerr := os.Remove(s.legacy); rerr != nil && !os.IsNotExist(rerr) {
		return errors.Wrap(rerr, "remove legacy session")
	}
	return nil
}
