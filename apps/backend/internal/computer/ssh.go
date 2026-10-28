// Package computer executes bounded requests over authenticated SSH. The remote
// helper is a CLI, not a service; detached jobs survive loss of the SSH session.
package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
	"openalbert/apps/backend/internal/domain"
)

type Config struct{ Address, User, Password, KeyFile string }
type Client struct {
	address string
	config  *ssh.ClientConfig
	runner  string
}

func New(c Config) (*Client, error) {
	if (c.Password == "") == (c.KeyFile == "") {
		return nil, errors.New("set exactly one of COMPUTER_SSH_PASSWORD or COMPUTER_SSH_KEY_FILE")
	}
	auth := ssh.Password(c.Password)
	if c.KeyFile != "" {
		key, err := os.ReadFile(c.KeyFile)
		if err != nil {
			return nil, errors.New("cannot read computer SSH identity")
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, errors.New("invalid computer SSH identity")
		}
		auth = ssh.PublicKeys(signer)
	}
	// User-managed computers intentionally do not require known_hosts or pinning.
	return &Client{address: c.Address, config: &ssh.ClientConfig{User: c.User, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: ssh.InsecureIgnoreHostKey()}, runner: runnerSource}, nil
}

// Limit transport time independently of the remote job's execution timeout.
// Closing a connection cancels observation, never the detached remote job.
func (c *Client) Execute(ctx context.Context, req domain.ComputerRequest) (domain.ComputerResult, error) {
	var out domain.ComputerResult
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", c.address)
	if err != nil {
		return out, errors.New("computer SSH connection unavailable; inspect the same execution ID before starting another command")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	sshConn, channels, requests, err := ssh.NewClientConn(conn, c.address, c.config)
	if err != nil {
		return out, errors.New("computer SSH authentication failed")
	}
	client := ssh.NewClient(sshConn, channels, requests)
	defer client.Close()
	command, err := prepareRunner(client, c.runner)
	if err != nil {
		return out, err
	}
	session, err := client.NewSession()
	if err != nil {
		return out, errors.New("cannot open computer SSH session")
	}
	defer session.Close()
	raw, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	session.Stdin = bytes.NewReader(raw)
	response := &boundedBuffer{limit: 30 << 20}
	session.Stdout = response
	session.Stderr = io.Discard // Never echo remote diagnostics or credentials into transport errors.
	if err = session.Run(command); err != nil {
		return out, errors.New("computer response lost; use computer_execution_status with the same execution ID; do not blindly repeat the command")
	}
	if response.overflow || json.Unmarshal(response.Bytes(), &out) != nil {
		return out, errors.New("invalid computer response; inspect the existing execution ID")
	}
	if out.Error != "" {
		return out, errors.New(out.Error)
	}
	return out, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
