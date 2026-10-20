package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// isTerminal reports whether f is a character device (a TTY).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// readSecret reads a value without echo when stdin is a terminal, or the
// whole of stdin (trailing newline trimmed) when it is piped.
func readSecret(prompt string, stderr io.Writer) (string, error) {
	if !isTerminal(os.Stdin) {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	fmt.Fprint(stderr, prompt)
	if err := stty("-echo"); err != nil {
		return "", fmt.Errorf("disable echo: %w", err)
	}
	defer func() {
		_ = stty("echo")
		fmt.Fprintln(stderr)
	}()
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func stty(arg string) error {
	cmd := exec.Command("stty", arg) //nolint:gosec // arg is the constant "echo" or "-echo"
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// confirm asks a yes/no question; non-interactive callers must pass --yes.
func confirm(in io.Reader, out io.Writer, question string) (bool, error) {
	if f, ok := in.(*os.File); ok && !isTerminal(f) {
		return false, errors.New("refusing to change anything without consent in non-interactive mode; pass --yes")
	}
	fmt.Fprintf(out, "%s [y/N] ", question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// passwordPrompt returns a Password callback for the master-key file
// fallback; it asks twice when creating.
func passwordPrompt(stderr io.Writer, creating bool) func() (string, error) {
	return func() (string, error) {
		if !isTerminal(os.Stdin) {
			return "", nil
		}
		fmt.Fprintln(stderr, "No OS secret store is available; the master key will be kept in a password-protected file.")
		pw, err := readSecret("Password: ", stderr)
		if err != nil || !creating || pw == "" {
			return pw, err
		}
		again, err := readSecret("Repeat password: ", stderr)
		if err != nil {
			return "", err
		}
		if again != pw {
			return "", errors.New("passwords do not match")
		}
		return pw, nil
	}
}
