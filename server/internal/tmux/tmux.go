package tmux

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// EnsureSession creates a tmux session with the given name if it does not exist.
func EnsureSession(slug string) error {
	err := run("tmux", "has-session", "-t", slug)
	if err == nil {
		return nil
	}
	return run("tmux", "new-session", "-d", "-s", slug)
}

// CreateWindow creates a new window in the given session with the specified
// name and starting directory.
func CreateWindow(session, name, startDir string) error {
	return run("tmux", "new-window", "-t", session, "-n", name, "-c", startDir)
}

// SendKeys sends keys to a window's pane. The text is sent literally via -l,
// then a separate Enter key event is sent to submit the input. Embedded
// newlines in keys are sent as literal 0x0A and are suitable for multiline
// stdin payloads; the final Enter terminates the input sequence.
func SendKeys(session, window, keys string) error {
	target := session + ":" + window
	if err := run("tmux", "send-keys", "-t", target, "-l", keys); err != nil {
		return err
	}
	return run("tmux", "send-keys", "-t", target, "Enter")
}

// KillWindow terminates a window in a session.
func KillWindow(session, window string) error {
	target := session + ":" + window
	return run("tmux", "kill-window", "-t", target)
}

// IsWindowAlive reports whether a window with the given name exists in the
// session. Returns (false, nil) when the session does not exist, and
// (false, err) for unexpected errors.
func IsWindowAlive(session, window string) (bool, error) {
	out, err := output("tmux", "list-windows", "-t", session, "-F", "#{window_name}")
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "no such session") || strings.Contains(msg, "can't find session") {
			return false, nil
		}
		return false, err
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if scanner.Text() == window {
			return true, nil
		}
	}
	return false, nil
}

// IsPaneIdle reports whether the pane's foreground process is a shell
// (bash, zsh, sh, fish, dash, ksh, tcsh, or nu), indicating that the
// harness process has exited.
func IsPaneIdle(session, window string) (bool, error) {
	target := session + ":" + window
	out, err := output("tmux", "display-message", "-t", target, "-p", "#{pane_current_command}")
	if err != nil {
		return false, fmt.Errorf("display-message: %w", err)
	}
	switch strings.TrimSpace(out) {
	case "bash", "zsh", "sh", "fish", "dash", "ksh", "tcsh", "nu":
		return true, nil
	}
	return false, nil
}

// PipeOutput redirects pane output to a temporary file and streams it line by
// line on the returned channel. Call the returned cleanup function to stop
// streaming and release resources. Cleanup is safe to call multiple times.
func PipeOutput(session, window string) (<-chan string, func(), error) {
	target := session + ":" + window

	f, err := os.CreateTemp("", "ccx-pipe-*.log")
	if err != nil {
		return nil, nil, err
	}
	logPath := f.Name()
	f.Close()

	// Shell-safe path: wrap in single quotes, escaping any embedded single quotes.
	safeLogPath := "'" + strings.ReplaceAll(logPath, "'", "'\\''") + "'"
	if err := run("tmux", "pipe-pane", "-t", target,
		"cat >> "+safeLogPath); err != nil {
		os.Remove(logPath)
		return nil, nil, fmt.Errorf("pipe-pane: %w", err)
	}

	ch := make(chan string, 128)
	stop := make(chan struct{})
	var once sync.Once

	go func() {
		defer close(ch)
		defer os.Remove(logPath)
		defer func() {
			// Stop pipe-pane (run with no command argument clears the pipe).
			_ = run("tmux", "pipe-pane", "-t", target)
		}()

		f, err := os.Open(logPath)
		if err != nil {
			return
		}
		defer f.Close()

		// Use bufio.Reader rather than bufio.Scanner: Scanner sets an internal
		// done flag on the first io.EOF and never reads the file again, making
		// it unsuitable for tailing a growing file. Reader.ReadString returns
		// io.EOF on every read that reaches the current end of file.
		//
		// Partial-line handling: ReadString('\n') may return (partial, io.EOF)
		// when the writer hasn't flushed the trailing newline yet. We accumulate
		// fragments in a strings.Builder and only emit a line once a '\n' is
		// seen (err == nil), avoiding split messages to consumers.
		// Blank lines are emitted as empty strings; callers may filter them.
		reader := bufio.NewReader(f)
		var buf strings.Builder
		for {
			select {
			case <-stop:
				return
			default:
			}
			fragment, err := reader.ReadString('\n')
			buf.WriteString(fragment)
			if err != nil {
				if errors.Is(err, io.EOF) {
					// Transient EOF — file still being written. Sleep and retry.
					time.Sleep(50 * time.Millisecond)
					continue
				}
				// Real read error — stop streaming.
				return
			}
			// Complete line received (ends with '\n').
			line := strings.TrimRight(buf.String(), "\r\n")
			buf.Reset()
			select {
			case ch <- line:
			case <-stop:
				return
			}
		}
	}()

	cleanup := func() {
		once.Do(func() { close(stop) })
	}
	return ch, cleanup, nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func output(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("%s %v: %w", name, args, err)
	}
	return string(out), nil
}
