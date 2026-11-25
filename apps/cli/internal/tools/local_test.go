package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLocalFilesystemGlobGrepAndBash(t *testing.T) {
	dir := t.TempDir()
	l := &Local{CWD: dir}

	// Write
	res := l.Execute(context.Background(), "fs_write", json.RawMessage(`{"path":"pkg/hello.go","content":"package pkg\n\nfunc Hello() string { return \"hi\" }\n"}`), nil)
	if res.Failed || res.Diff == nil || res.Diff.OldText != "" || !strings.Contains(res.Diff.NewText, "func Hello") {
		t.Fatalf("fs_write failed: %+v %s", res, res.Output)
	}

	// Read range
	res = l.Execute(context.Background(), "fs_read", json.RawMessage(`{"path":"pkg/hello.go","start_line":3,"end_line":3}`), nil)
	if res.Failed || !strings.Contains(string(res.Output), `func Hello()`) {
		t.Fatalf("fs_read failed: %s", res.Output)
	}

	// Edit
	res = l.Execute(context.Background(), "fs_edit", json.RawMessage(`{"path":"pkg/hello.go","old_text":"return \"hi\"","new_text":"return \"hello world\""}`), nil)
	if res.Failed || res.Diff == nil || !strings.Contains(res.Diff.NewText, "hello world") {
		t.Fatalf("fs_edit failed: %s", res.Output)
	}

	// Glob
	res = l.Execute(context.Background(), "fs_glob", json.RawMessage(`{"pattern":"**/*.go"}`), nil)
	if res.Failed || !strings.Contains(string(res.Output), "pkg/hello.go") {
		t.Fatalf("fs_glob failed: %s", res.Output)
	}

	// Grep
	res = l.Execute(context.Background(), "fs_grep", json.RawMessage(`{"pattern":"hello world","fixed_strings":true}`), nil)
	if res.Failed || !strings.Contains(string(res.Output), "pkg/hello.go") {
		t.Fatalf("fs_grep failed: %s", res.Output)
	}

	// Bash with streaming + cancel
	var streamed strings.Builder
	res = l.Execute(context.Background(), "bash", json.RawMessage(`{"command":"printf 'line1\\nline2\\n'"}`), func(chunk string) {
		streamed.WriteString(chunk)
	})
	if res.Failed || !strings.Contains(streamed.String(), "line1") || !strings.Contains(string(res.Output), `"exit_code":0`) {
		t.Fatalf("bash failed: %s streamed=%q", res.Output, streamed.String())
	}

	// Verify file persisted
	raw, err := os.ReadFile(filepath.Join(dir, "pkg", "hello.go"))
	if err != nil || !strings.Contains(string(raw), "hello world") {
		t.Fatalf("persisted file unexpected: %q %v", string(raw), err)
	}
}

func TestLocalWriteAndEditValidateRequiredStringFields(t *testing.T) {
	dir := t.TempDir()
	l := &Local{CWD: dir}

	res := l.Execute(context.Background(), "fs_write", json.RawMessage(`{"path":"note.txt","content":"keep me"}`), nil)
	if res.Failed {
		t.Fatalf("initial fs_write failed: %s", res.Output)
	}
	// Missing content must fail without truncating the existing file.
	res = l.Execute(context.Background(), "fs_write", json.RawMessage(`{"path":"note.txt"}`), nil)
	if !res.Failed {
		t.Fatalf("fs_write without content should fail, got %s", res.Output)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "note.txt"))
	if err != nil || string(raw) != "keep me" {
		t.Fatalf("file was modified on rejected fs_write: %q %v", string(raw), err)
	}
	// Missing new_text must fail without deleting matched text.
	res = l.Execute(context.Background(), "fs_edit", json.RawMessage(`{"path":"note.txt","old_text":"keep"}`), nil)
	if !res.Failed {
		t.Fatalf("fs_edit without new_text should fail, got %s", res.Output)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "note.txt"))
	if err != nil || string(raw) != "keep me" {
		t.Fatalf("file was modified on rejected fs_edit: %q %v", string(raw), err)
	}
	// Explicit empty new_text is allowed for intentional deletion.
	res = l.Execute(context.Background(), "fs_edit", json.RawMessage(`{"path":"note.txt","old_text":"keep ","new_text":""}`), nil)
	if res.Failed {
		t.Fatalf("fs_edit with explicit empty new_text failed: %s", res.Output)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "note.txt"))
	if err != nil || string(raw) != "me" {
		t.Fatalf("expected deletion with explicit empty new_text, got %q %v", string(raw), err)
	}
}

func TestLocalGrepSmallLimitWithManyMatchesDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	var content strings.Builder
	for i := 0; i < 4000; i++ {
		content.WriteString("needle_match_line_content_padding_0123456789abcdef\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(content.String()), 0644); err != nil {
		t.Fatal(err)
	}
	l := &Local{CWD: dir}
	res := l.Execute(context.Background(), "fs_grep", json.RawMessage(`{"pattern":"needle_match","fixed_strings":true,"limit":2}`), nil)
	if res.Failed {
		t.Fatalf("fs_grep failed: %s", res.Output)
	}
	var out struct {
		Matches   []any `json:"matches"`
		Truncated bool  `json:"truncated"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("unmarshal grep output: %v (%s)", err, res.Output)
	}
	if len(out.Matches) != 2 || !out.Truncated {
		t.Fatalf("expected 2 matches with truncated=true, got %d truncated=%v", len(out.Matches), out.Truncated)
	}
}

func TestLocalReadFileAppliesLimitToSelectedRange(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	const totalNonEmptyLines = 600
	pad := strings.Repeat("x", 1000)
	var line521 string
	for i := 1; i <= totalNonEmptyLines; i++ {
		line := pad + "-line-" + string(rune('0'+(i/100)%10)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10))
		if i == 521 {
			line521 = line
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if sb.Len() <= maxReadBytes {
		t.Fatalf("test file too small: %d <= %d", sb.Len(), maxReadBytes)
	}
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}

	l := &Local{CWD: dir}
	res := l.Execute(context.Background(), "fs_read", json.RawMessage(`{"path":"large.txt","start_line":521,"end_line":521}`), nil)
	if res.Failed {
		t.Fatalf("fs_read line 521 failed: %s", res.Output)
	}
	var out struct {
		TotalLines int    `json:"total_lines"`
		StartLine  int    `json:"start_line"`
		EndLine    int    `json:"end_line"`
		Content    string `json:"content"`
		Truncated  bool   `json:"truncated"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("unmarshal fs_read output: %v (%s)", err, res.Output)
	}
	if out.TotalLines != totalNonEmptyLines+1 {
		t.Fatalf("total_lines=%d, want %d", out.TotalLines, totalNonEmptyLines+1)
	}
	if out.Content != line521 {
		t.Fatalf("line 521 content mismatch: got len=%d want len=%d", len(out.Content), len(line521))
	}
	if out.Truncated {
		t.Fatalf("expected truncated=false for single-line range beyond 512 KiB offset")
	}

	// Reading the entire >512 KiB file should still truncate the selected range to maxReadBytes.
	fullRes := l.Execute(context.Background(), "fs_read", json.RawMessage(`{"path":"large.txt"}`), nil)
	if fullRes.Failed {
		t.Fatalf("fs_read full file failed: %s", fullRes.Output)
	}
	var fullOut struct {
		TotalLines int    `json:"total_lines"`
		Content    string `json:"content"`
		Truncated  bool   `json:"truncated"`
	}
	if err := json.Unmarshal(fullRes.Output, &fullOut); err != nil {
		t.Fatalf("unmarshal full fs_read output: %v", err)
	}
	if !fullOut.Truncated || len(fullOut.Content) > maxReadBytes || fullOut.TotalLines != totalNonEmptyLines+1 {
		t.Fatalf("unexpected full fs_read result: truncated=%v len=%d total_lines=%d", fullOut.Truncated, len(fullOut.Content), fullOut.TotalLines)
	}
}

func TestLocalReadFileRejectsFIFOAndHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "pipe.fifo")
	if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	l := &Local{CWD: dir}
	res := l.Execute(context.Background(), "fs_read", json.RawMessage(`{"path":"pipe.fifo"}`), nil)
	if !res.Failed || !strings.Contains(string(res.Output), "non-regular") {
		t.Fatalf("expected fs_read on FIFO to fail immediately without blocking, got failed=%v out=%s", res.Failed, res.Output)
	}

	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelRes := l.Execute(cancelledCtx, "fs_read", json.RawMessage(`{"path":"plain.txt"}`), nil)
	if !cancelRes.Failed || !strings.Contains(string(cancelRes.Output), "cancelled") {
		t.Fatalf("expected fs_read with cancelled context to fail, got failed=%v out=%s", cancelRes.Failed, cancelRes.Output)
	}
}

func TestLocalGrepFallbackSkipsFIFOAndOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "hang.fifo")
	if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "match.txt"), []byte("hello target_needle here\n"), 0644); err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("z", (2<<20)+1024) + "\ntarget_needle\n"
	if err := os.WriteFile(filepath.Join(dir, "huge.txt"), []byte(oversized), 0644); err != nil {
		t.Fatal(err)
	}

	l := &Local{CWD: dir}
	res := l.grepFallback(context.Background(), dir, "target_needle", "", true, false, 10)
	if res.Failed {
		t.Fatalf("grepFallback failed: %s", res.Output)
	}
	var out struct {
		Matches []struct {
			Path string `json:"path"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("unmarshal grepFallback output: %v (%s)", err, res.Output)
	}
	if len(out.Matches) != 1 || out.Matches[0].Path != "match.txt" {
		t.Fatalf("expected only match.txt (skipping FIFO and >2MiB file), got %+v", out.Matches)
	}
}

func TestLocalWriteAndEditRejectFIFOAndHonorCancellation(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "pipe.fifo")
	if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	l := &Local{CWD: dir}

	wRes := l.Execute(context.Background(), "fs_write", json.RawMessage(`{"path":"pipe.fifo","content":"x"}`), nil)
	if !wRes.Failed || !strings.Contains(string(wRes.Output), "non-regular") {
		t.Fatalf("expected fs_write on FIFO to fail immediately, got failed=%v out=%s", wRes.Failed, wRes.Output)
	}
	eRes := l.Execute(context.Background(), "fs_edit", json.RawMessage(`{"path":"pipe.fifo","old_text":"a","new_text":"b"}`), nil)
	if !eRes.Failed || !strings.Contains(string(eRes.Output), "non-regular") {
		t.Fatalf("expected fs_edit on FIFO to fail immediately, got failed=%v out=%s", eRes.Failed, eRes.Output)
	}

	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := l.Execute(cctx, "fs_write", json.RawMessage(`{"path":"file.txt","content":"updated"}`), nil); !r.Failed || !strings.Contains(string(r.Output), "cancelled") {
		t.Fatalf("expected cancelled fs_write to fail, got failed=%v out=%s", r.Failed, r.Output)
	}
	if r := l.Execute(cctx, "fs_edit", json.RawMessage(`{"path":"file.txt","old_text":"hello","new_text":"bye"}`), nil); !r.Failed || !strings.Contains(string(r.Output), "cancelled") {
		t.Fatalf("expected cancelled fs_edit to fail, got failed=%v out=%s", r.Failed, r.Output)
	}
}
