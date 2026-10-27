package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/persistence"
)

// runUserCommand implements `worker user set-password`. It is the only way to
// create or reset the single operator account: no default password exists and
// the API has no signup route.
func runUserCommand(args []string) {
	if len(args) == 0 || args[0] != "set-password" {
		fmt.Fprintln(os.Stderr, "usage: worker user set-password [--username operator]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("user set-password", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	username := fs.String("username", "operator", "account to create or reset")
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: worker user set-password [--username operator]")
		os.Exit(2)
	}

	password, err := promptPassword(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read password: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	cfg := config.Load()
	database, err := persistence.NewDatabase(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	auth := application.NewAuthService(
		persistence.NewPgAuthStore(database.Queries),
		application.SystemClock{}, cfg.SessionTTL, nil, nil,
	)
	user, err := auth.SetPassword(ctx, *username, password)
	switch {
	case errors.Is(err, application.ErrWeakPassword), errors.Is(err, application.ErrUsernameInvalid):
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	case err != nil:
		fmt.Fprintf(os.Stderr, "set password: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "password set for %q; all existing sessions revoked\n", user.Username)
}

// promptPassword reads VTX_USER_PASSWORD for automation, or prompts twice on a
// terminal with echo disabled. Passwords are never accepted as flags: argv is
// visible in process listings and shell history.
func promptPassword(stdin *os.File) (string, error) {
	if value := os.Getenv("VTX_USER_PASSWORD"); value != "" {
		return value, nil
	}
	if !isTerminal(int(stdin.Fd())) {
		return readPasswordLine(stdin)
	}

	reader := bufio.NewReader(stdin)
	first, err := readSecretLine("New password: ", reader, stdin)
	if err != nil {
		return "", err
	}
	second, err := readSecretLine("Confirm password: ", reader, stdin)
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("passwords do not match")
	}
	return first, nil
}

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

func readSecretLine(prompt string, reader *bufio.Reader, stdin *os.File) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	old, err := unix.IoctlGetTermios(int(stdin.Fd()), unix.TCGETS)
	if err != nil {
		return "", err
	}
	silent := *old
	silent.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(int(stdin.Fd()), unix.TCSETS, &silent); err != nil {
		return "", err
	}
	line, readErr := reader.ReadString('\n')
	if err := unix.IoctlSetTermios(int(stdin.Fd()), unix.TCSETS, old); err != nil {
		return "", err
	}
	fmt.Fprintln(os.Stderr)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return "", readErr
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func readPasswordLine(reader io.Reader) (string, error) {
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
