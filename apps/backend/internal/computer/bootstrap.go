package computer

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/ssh"
)

// The backend carries its matching runner, including in release images.
//
//go:embed runner.py
var runnerSource string

//go:embed bootstrap.py
var bootstrapSource string

func runnerDigest(source string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(source))) }

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func prepareRunner(client *ssh.Client, source string) (string, error) {
	digest := runnerDigest(source)
	for _, action := range []string{"check", "install"} {
		session, err := client.NewSession()
		if err != nil {
			return "", errors.New("cannot open computer preparation session; no command dispatched")
		}
		response := &boundedBuffer{limit: 4096}
		session.Stdout = response
		session.Stderr = io.Discard
		if action == "install" {
			session.Stdin = strings.NewReader(source)
		}
		command := "if command -v python3 >/dev/null 2>&1; then python3 -c " + shellQuote(bootstrapSource) + " " + digest + " " + action + "; else printf '%s\\n' '{\"error\":\"python_required\"}'; fi"
		err = session.Run(command)
		_ = session.Close()
		if err != nil || response.overflow {
			return "", errors.New("computer preparation failed; no command dispatched")
		}
		var result struct{ State, Error string }
		if json.Unmarshal(response.Bytes(), &result) != nil {
			return "", errors.New("invalid computer preparation response; no command dispatched")
		}
		if result.Error != "" {
			detail := map[string]string{
				"linux_required":    "Linux is required",
				"python_required":   "install Python 3.9 or newer on the computer",
				"python_version":    "install Python 3.9 or newer on the computer",
				"bash_required":     "install Bash at /bin/bash on the computer",
				"home_not_writable": "the SSH user needs write access to ~/.openalbert",
			}[result.Error]
			if detail == "" {
				detail = "check available disk space and write access to ~/.openalbert"
			}
			return "", errors.New("computer preparation failed: " + detail + "; no command dispatched")
		}
		if result.State == "ready" {
			// Immutable paths keep old supervisors and not-yet-spawned workers on
			// their own source version. Job records and machine.lock stay shared.
			return `python3 "$HOME/.openalbert/runners/` + digest + `.py"`, nil
		}
		if result.State != "install" || action == "install" {
			return "", errors.New("invalid computer preparation state; no command dispatched")
		}
	}
	return "", errors.New("computer preparation failed; no command dispatched")
}
