package computer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"openalbert/apps/backend/internal/domain"
)

func TestSSHPasswordPreparationAndDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, check, install   string
		wantInstall, wantError bool
	}{
		{name: "first connection", check: `{"state":"install"}`, install: `{"state":"ready"}`, wantInstall: true},
		{name: "already installed", check: `{"state":"ready"}`},
		{name: "missing dependency", check: `{"error":"python_required"}`, wantError: true},
		{name: "failed install", check: `{"state":"install"}`, install: `{"error":"preparation_failed"}`, wantInstall: true, wantError: true},
		{name: "unexpected response", check: `{"state":"garbage"}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commands := make(chan string, 8)
			address := testSSHServer(t, func(command string, input []byte) string {
				commands <- command
				if strings.HasSuffix(command, " check; else printf '%s\\n' '{\"error\":\"python_required\"}'; fi") {
					return tc.check
				}
				if strings.Contains(command, " install; else") {
					if string(input) != runnerSource {
						t.Error("incorrect runner uploaded")
					}
					return tc.install
				}
				if command != `python3 "$HOME/.openalbert/runners/`+runnerDigest(runnerSource)+`.py"` {
					t.Error("wrong runner command")
				}
				var request domain.ComputerRequest
				if json.Unmarshal(input, &request) != nil || request.Action != "status" {
					t.Error("incorrect dispatch")
				}
				return `{"state":"unknown"}`
			})
			c, err := New(Config{Address: address, User: "test-user", Password: " synthetic-password "})
			if err != nil {
				t.Fatal(err)
			}
			out, err := c.Execute(context.Background(), domain.ComputerRequest{Action: "status"})
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if err == nil && out.State != "unknown" {
				t.Fatal("result lost")
			}
			want := 1
			if tc.wantInstall {
				want++
			}
			if !tc.wantError {
				want++
			}
			if len(commands) != want {
				t.Fatal("command dispatched before successful preparation")
			}
		})
	}
}

func testSSHServer(t *testing.T, respond func(string, []byte) string) string {
	t.Helper()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	host, _ := ssh.NewSignerFromKey(private)
	server := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if meta.User() != "test-user" || string(password) != " synthetic-password " {
			return nil, fmt.Errorf("unexpected credentials")
		}
		return nil, nil
	}}
	server.AddHostKey(host)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		serverConn, channels, requests, err := ssh.NewServerConn(conn, server)
		if err != nil {
			return
		}
		defer serverConn.Close()
		go ssh.DiscardRequests(requests)
		for channel := range channels {
			session, requests, err := channel.Accept()
			if err != nil {
				return
			}
			for request := range requests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				_ = ssh.Unmarshal(request.Payload, &payload)
				_ = request.Reply(true, nil)
				input, _ := io.ReadAll(session)
				_, _ = io.WriteString(session, respond(payload.Command, input))
				_, _ = session.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = session.Close()
				break
			}
		}
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("SSH fixture did not stop")
		}
	})
	return listener.Addr().String()
}

func TestSSHAuthenticationAndCancellation(t *testing.T) {
	for _, cfg := range []Config{{}, {Password: "secret", KeyFile: "private-path"}, {KeyFile: "missing-private-path"}} {
		if _, err := New(cfg); err == nil || strings.Contains(err.Error(), "private-path") || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid auth accepted or leaked")
		}
	}
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.MarshalPrivateKey(private, "test")
	keyPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(key), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{User: "test-user", KeyFile: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	// A peer that accepts TCP but never speaks SSH must not pin a worker.
	listener2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener2.Close()
	go func() {
		conn, e := listener2.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		time.Sleep(200 * time.Millisecond)
	}()
	c.address = listener2.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = c.Execute(ctx, domain.ComputerRequest{Action: "status"})
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestTransportBoundsOutput(t *testing.T) {
	b := &boundedBuffer{limit: 8}
	if n, err := b.Write(bytes.Repeat([]byte("x"), 20)); n != 20 || err != nil || !b.overflow || b.Len() != 8 {
		t.Fatal("unbounded output")
	}
}

func realClient(t *testing.T) *Client {
	t.Helper()
	address := os.Getenv("OPENALBERT_TEST_COMPUTER_ADDRESS")
	if address == "" {
		t.Skip("set OPENALBERT_TEST_COMPUTER_ADDRESS using tools/smoke/computer-smoke.py")
	}
	c, err := New(Config{Address: address, User: "albert", KeyFile: os.Getenv("OPENALBERT_TEST_COMPUTER_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func execute(t *testing.T, c *Client, r domain.ComputerRequest) domain.ComputerResult {
	t.Helper()
	out, err := c.Execute(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func await(t *testing.T, c *Client, id string) domain.ComputerResult {
	t.Helper()
	until := time.Now().Add(12 * time.Second)
	for time.Now().Before(until) {
		out := execute(t, c, domain.ComputerRequest{Action: "status", ExecutionID: id})
		if out.State != "queued" && out.State != "running" {
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job never finished")
	return domain.ComputerResult{}
}
func execRequest(id, command string) domain.ComputerRequest {
	return domain.ComputerRequest{Action: "exec", ExecutionID: id, Command: command, Cwd: "/workspace", TimeoutSeconds: 20, WaitSeconds: 1}
}

func TestRealComputer(t *testing.T) {
	c := realClient(t)
	t.Run("first connection, home workspace and immutable updates", func(t *testing.T) {
		out := execute(t, c, domain.ComputerRequest{Action: "exec", ExecutionID: "dddddddddddddddddddddddddddddddd", Command: `pwd; test ! -e /usr/local/bin/openalbert-computer`, TimeoutSeconds: 20, WaitSeconds: 1})
		if out.State != "completed" || out.Stdout != "/home/albert/.openalbert/workspace\n" {
			t.Fatalf("automatic installation/default cwd failed: %+v", out)
		}
		old := execRequest("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", `echo started; sleep 30; touch update-should-not-exist`)
		out = execute(t, c, old)
		if out.State != "running" || out.Stdout != "started\n" {
			t.Fatalf("old version did not start: %+v", out)
		}
		updated := *c
		updated.runner += "\n# synthetic updated runner\n"
		queued := execRequest("ffffffffffffffffffffffffffffffff", `printf updated`)
		queued.WaitSeconds = 0
		out = execute(t, &updated, queued)
		if out.State != "queued" {
			t.Fatalf("update lost shared machine lock: %+v", out)
		}
		for _, source := range []string{c.runner, updated.runner} {
			file := execute(t, &updated, domain.ComputerRequest{Action: "download", Path: "/home/albert/.openalbert/runners/" + runnerDigest(source) + ".py"})
			if string(file.Data) != source {
				t.Fatal("update replaced or corrupted a runner version")
			}
		}
		out = execute(t, &updated, domain.ComputerRequest{Action: "status", ExecutionID: old.ExecutionID})
		if out.State != "running" {
			t.Fatalf("update interrupted old worker: %+v", out)
		}
		out = execute(t, &updated, domain.ComputerRequest{Action: "cancel", ExecutionID: old.ExecutionID})
		if out.State != "cancelled" {
			t.Fatalf("new runner cannot cancel old worker: %+v", out)
		}
		if out = await(t, &updated, queued.ExecutionID); out.State != "completed" || out.Stdout != "updated" {
			t.Fatalf("new worker failed: %+v", out)
		}
		if out = execute(t, &updated, old); out.State != "cancelled" {
			t.Fatal("update replayed an existing execution")
		}
	})
	t.Run("sudo, binary files and replay", func(t *testing.T) {
		id := "11111111111111111111111111111111"
		r := execRequest(id, `printf 'one\n' >> counter; sudo -n id -u; printf 'error' >&2`)
		out := execute(t, c, r)
		if out.State == "running" || out.State == "queued" {
			out = await(t, c, id)
		}
		if out.State != "completed" || out.Stdout != "0\n" || out.Stderr != "error" {
			t.Fatalf("bad result: %+v", out)
		}
		execute(t, c, r) // a lost Activity response must not append twice
		check := execute(t, c, execRequest("22222222222222222222222222222222", `cat counter`))
		if check.Stdout != "one\n" {
			t.Fatalf("command repeated: %+v", check)
		}
		data := []byte{0, 1, 2, 255, 128, 10}
		execute(t, c, domain.ComputerRequest{Action: "upload", Path: "/workspace/binary.dat", Data: data})
		execute(t, c, domain.ComputerRequest{Action: "upload", Path: "/workspace/binary.dat", Data: data})
		download := execute(t, c, domain.ComputerRequest{Action: "download", Path: "/workspace/binary.dat"})
		if !bytes.Equal(download.Data, data) {
			t.Fatal("binary transfer corrupted")
		}
		if _, err := c.Execute(context.Background(), domain.ComputerRequest{Action: "upload", Path: "/workspace/binary.dat", Data: []byte("overwrite")}); err == nil {
			t.Fatal("changed file overwritten")
		}
		if _, err := c.Execute(context.Background(), domain.ComputerRequest{Action: "download", Path: "/etc/passwd"}); err == nil {
			t.Fatal("transfer escaped workspace")
		}
	})
	t.Run("timeout and queue cancellation", func(t *testing.T) {
		id := "33333333333333333333333333333333"
		r := execRequest(id, `sleep 30; touch /workspace/should-not-exist`)
		r.TimeoutSeconds = 1
		r.WaitSeconds = 0
		execute(t, c, r)
		queued := "44444444444444444444444444444444"
		r2 := execRequest(queued, `touch /workspace/queued-should-not-exist`)
		r2.WaitSeconds = 0
		out := execute(t, c, r2)
		if out.State != "queued" {
			t.Fatalf("commands not serialized: %+v", out)
		}
		out = execute(t, c, domain.ComputerRequest{Action: "cancel", ExecutionID: queued})
		if out.State != "cancelled" {
			t.Fatalf("queued cancellation failed: %+v", out)
		}
		out = await(t, c, id)
		if out.State != "timed_out" {
			t.Fatalf("timeout ignored: %+v", out)
		}
		check := execute(t, c, execRequest("55555555555555555555555555555555", `test ! -e should-not-exist && test ! -e queued-should-not-exist`))
		if check.State != "completed" {
			t.Fatalf("cancelled command took effect: %+v", check)
		}
	})
	t.Run("running cancellation and output bounds", func(t *testing.T) {
		id := "66666666666666666666666666666666"
		r := execRequest(id, `sleep 30; touch /workspace/cancelled-should-not-exist`)
		r.WaitSeconds = 0
		execute(t, c, r)
		time.Sleep(150 * time.Millisecond)
		out := execute(t, c, domain.ComputerRequest{Action: "cancel", ExecutionID: id})
		if out.State != "cancelled" {
			t.Fatalf("cancel failed: %+v", out)
		}
		id = "77777777777777777777777777777777"
		r = execRequest(id, `python3 -c 'import sys; sys.stdout.write("x" * 2000000); sys.stderr.write("y" * 2000000)'`)
		execute(t, c, r)
		out = await(t, c, id)
		if !out.OutputTruncated || !out.HasMore || len(out.Stdout) > 2048 || len(out.Stderr) > 2048 {
			t.Fatalf("unbounded output: %+v", out)
		}
		next := execute(t, c, domain.ComputerRequest{Action: "status", ExecutionID: id, StdoutOffset: out.StdoutOffset, StderrOffset: out.StderrOffset})
		if next.StdoutOffset <= out.StdoutOffset || !next.HasMore {
			t.Fatal("output cursor failed")
		}
	})
	t.Run("privileged process timeout", func(t *testing.T) {
		id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		r := execRequest(id, `sudo -n bash -c 'sleep 30; touch /workspace/root-should-not-exist'`)
		r.TimeoutSeconds = 1
		r.WaitSeconds = 0
		execute(t, c, r)
		out := await(t, c, id)
		if out.State != "timed_out" {
			t.Fatalf("sudo process timeout failed: %+v", out)
		}
	})
	t.Run("connection loss after dispatch", func(t *testing.T) {
		id := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		r := execRequest(id, `printf 'once\n' >> disconnect-counter; sleep 1`)
		r.WaitSeconds = 5
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		if _, err := c.Execute(ctx, r); err == nil {
			t.Fatal("expected interrupted observation")
		}
		// Reconnecting with the SAME ID observes the surviving worker.
		r.WaitSeconds = 0
		execute(t, c, r)
		if out := await(t, c, id); out.State != "completed" {
			t.Fatalf("SSH disconnect killed command: %+v", out)
		}
		out := execute(t, c, execRequest("cccccccccccccccccccccccccccccccc", `cat disconnect-counter`))
		if out.Stdout != "once\n" {
			t.Fatalf("command duplicated after loss: %+v", out)
		}
	})
	t.Run("prepare disconnected execution", func(t *testing.T) {
		r := execRequest("88888888888888888888888888888888", `printf 'started\n' >> restart-counter; sleep 60`)
		r.WaitSeconds = 0
		out := execute(t, c, r)
		if out.State != "running" && out.State != "queued" {
			t.Fatal(out)
		}
	})
}

func TestRealComputerPassword(t *testing.T) {
	address, password := os.Getenv("OPENALBERT_TEST_COMPUTER_ADDRESS"), os.Getenv("OPENALBERT_TEST_COMPUTER_PASSWORD")
	if address == "" || password == "" {
		t.Skip("password smoke phase")
	}
	c, err := New(Config{Address: address, User: "worker", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	out := execute(t, c, domain.ComputerRequest{Action: "exec", ExecutionID: "01010101010101010101010101010101", Command: `id -un; pwd; ! sudo -n true 2>/dev/null`, TimeoutSeconds: 10, WaitSeconds: 1})
	if out.State != "completed" || out.Stdout != "worker\n/home/worker/.openalbert/workspace\n" {
		t.Fatalf("password execution failed: %+v", out)
	}
}

// The smoke harness recreates the container, retaining its home/workspace and
// connection identity, before invoking this phase in a new Go process.
func TestRealComputerAfterRestart(t *testing.T) {
	if os.Getenv("OPENALBERT_TEST_COMPUTER_RESTARTED") == "" {
		t.Skip("restart phase")
	}
	c := realClient(t)
	id := "88888888888888888888888888888888"
	out := execute(t, c, domain.ComputerRequest{Action: "status", ExecutionID: id})
	if out.State != "unknown" {
		t.Fatalf("interrupted execution misreported: %+v", out)
	}
	out = execute(t, c, execRequest("99999999999999999999999999999999", `cat counter; cat restart-counter; test -f binary.dat`))
	if out.State != "completed" || out.Stdout != "one\nstarted\n" {
		t.Fatalf("persistent files lost: %+v", out)
	}
	r := execRequest(id, `printf 'started\n' >> restart-counter; sleep 60`)
	r.WaitSeconds = 0
	out = execute(t, c, r)
	if out.State != "unknown" {
		t.Fatal("restarted ambiguous command")
	}
}
