package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mishankov/OpenAlbert/apps/acp/internal/serverclient"
)

const (
	maxReadBytes   = 512 << 10
	maxEditBytes   = 10 << 20
	maxBashOut     = 256 << 10
	defaultBashSec = 120
	maxBashSec     = 600
	maxGlobResults = 500
	maxGrepMatches = 200
)

// FileDiff records before/after text for ACP diff rendering.
type FileDiff struct {
	Path    string
	OldText string
	NewText string
}

// LocalResult holds the JSON payload returned to Codemode plus optional ACP UI
// metadata (file diffs, image attachments, file locations).
type LocalResult struct {
	Output    json.RawMessage
	Failed    bool
	Diff      *FileDiff
	Images    []string
	Locations []string
}

// OutputStreamer receives incremental stdout/stderr text from bash executions.
type OutputStreamer func(chunk string)

// Local implements local filesystem, search, and shell tools anchored to CWD.
type Local struct {
	CWD string
}

// Definitions returns the Codemode JSON schemas for all local coding tools.
func Definitions() []serverclient.Tool {
	schema := func(props string, req ...string) json.RawMessage {
		b, _ := json.Marshal(req)
		if req == nil {
			b = []byte("[]")
		}
		return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{` + props + `},"required":` + string(b) + `}`)
	}
	return []serverclient.Tool{
		{
			Name:        "fs_read",
			Description: "Read a file relative to the workspace or by absolute path. Supports optional 1-based start_line and end_line for text files, and returns image data for PNG/JPEG/WebP images.",
			Parameters:  schema(`"path":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}`, "path"),
		},
		{
			Name:        "fs_write",
			Description: "Create or overwrite a file with UTF-8 content, creating parent directories if needed.",
			Parameters:  schema(`"path":{"type":"string"},"content":{"type":"string"}`, "path", "content"),
		},
		{
			Name:        "fs_edit",
			Description: "Replace an exact old_text fragment with new_text in an existing file. Fails unless old_text matches once (or set replace_all=true to replace all occurrences).",
			Parameters:  schema(`"path":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"},"replace_all":{"type":"boolean"}`, "path", "old_text", "new_text"),
		},
		{
			Name:        "fs_glob",
			Description: "List files matching a glob pattern (supports **, *, ?) under the workspace or an optional root directory, skipping .git and node_modules.",
			Parameters:  schema(`"pattern":{"type":"string"},"root":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":500}`, "pattern"),
		},
		{
			Name:        "fs_grep",
			Description: "Search file contents by regex or fixed string under the workspace or an optional path. Uses ripgrep when installed with a built-in Go fallback.",
			Parameters:  schema(`"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"},"fixed_strings":{"type":"boolean"},"case_insensitive":{"type":"boolean"},"limit":{"type":"integer","minimum":1,"maximum":200}`, "pattern"),
		},
		{
			Name:        "bash",
			Description: "Run a shell command in the workspace (or custom cwd) with bash/sh. Supports timeout_seconds (default 120, max 600). Returns exit_code, stdout, stderr, and timed_out.",
			Parameters:  schema(`"command":{"type":"string"},"cwd":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":600}`, "command"),
		},
	}
}

// IsLocal reports whether a tool name is handled by Local.
func IsLocal(name string) bool {
	switch name {
	case "fs_read", "fs_write", "fs_edit", "fs_glob", "fs_grep", "bash":
		return true
	default:
		return false
	}
}

// IsReadOnly reports whether a local tool has no filesystem or system side effects.
func IsReadOnly(name string) bool {
	switch name {
	case "fs_read", "fs_glob", "fs_grep":
		return true
	default:
		return false
	}
}

// ACPKind maps tool names to ACP tool call kinds.
func ACPKind(name string) string {
	switch name {
	case "fs_read":
		return "read"
	case "fs_write", "fs_edit":
		return "edit"
	case "fs_glob", "fs_grep", "memory_search", "conversation_history", "logs_search", "task_find", "participant_search":
		return "search"
	case "bash", "computer_exec":
		return "execute"
	case "web_search", "web_read", "browser_open", "browser_navigate":
		return "fetch"
	default:
		return "other"
	}
}

func (l *Local) resolvePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is required")
	}
	if strings.ContainsRune(p, '\x00') {
		return "", errors.New("invalid path")
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Clean(filepath.Join(l.CWD, p)), nil
}

func failResult(msg, code string) LocalResult {
	raw, _ := json.Marshal(map[string]string{"error": msg, "error_code": code})
	return LocalResult{Output: raw, Failed: true}
}

func okResult(v any, diff *FileDiff, locations []string, images []string) LocalResult {
	raw, _ := json.Marshal(v)
	return LocalResult{Output: raw, Diff: diff, Locations: locations, Images: images}
}

// Execute runs one local coding tool call.
func (l *Local) Execute(ctx context.Context, name string, rawArgs json.RawMessage, stream OutputStreamer) LocalResult {
	switch name {
	case "fs_read":
		return l.readFile(ctx, rawArgs)
	case "fs_write":
		return l.writeFile(ctx, rawArgs)
	case "fs_edit":
		return l.editFile(ctx, rawArgs)
	case "fs_glob":
		return l.glob(ctx, rawArgs)
	case "fs_grep":
		return l.grep(ctx, rawArgs)
	case "bash":
		return l.runBash(ctx, rawArgs, stream)
	default:
		return failResult("unknown local tool: "+name, "unknown_tool")
	}
}

func (l *Local) readFile(ctx context.Context, rawArgs json.RawMessage) LocalResult {
	if err := ctx.Err(); err != nil {
		return failResult("read cancelled", "cancelled")
	}
	var args struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return failResult("invalid fs_read arguments", "invalid_request")
	}
	target, err := l.resolvePath(args.Path)
	if err != nil {
		return failResult(err.Error(), "invalid_request")
	}
	info, err := os.Stat(target)
	if err != nil {
		return failResult(err.Error(), "not_found")
	}
	if info.IsDir() {
		entries, err := os.ReadDir(target)
		if err != nil {
			return failResult(err.Error(), "io_error")
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() {
				n += "/"
			}
			names = append(names, n)
		}
		return okResult(map[string]any{"path": target, "is_dir": true, "entries": names}, nil, []string{target}, nil)
	}
	if !info.Mode().IsRegular() {
		return failResult(fmt.Sprintf("unsupported non-regular file type (%s)", info.Mode().Type()), "invalid_request")
	}
	start := args.StartLine
	if start <= 0 {
		start = 1
	}
	end := args.EndLine
	if end > 0 && end < start {
		return failResult("end_line must be >= start_line", "invalid_request")
	}

	f, err := os.Open(target)
	if err != nil {
		return failResult(err.Error(), "io_error")
	}
	defer f.Close()

	buf := make([]byte, 32<<10)
	var mimeType string
	var utf8Carry []byte
	var selected bytes.Buffer
	truncated := false
	lineNum := 1
	firstChunk := true

	appendBounded := func(b []byte) {
		if truncated || len(b) == 0 {
			return
		}
		if selected.Len()+len(b) <= maxReadBytes {
			selected.Write(b)
			return
		}
		if rem := maxReadBytes - selected.Len(); rem > 0 {
			selected.Write(b[:rem])
		}
		truncated = true
	}

	for {
		if err := ctx.Err(); err != nil {
			return failResult("read cancelled", "cancelled")
		}
		n, readErr := f.Read(buf)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return failResult(readErr.Error(), "io_error")
		}
		data := buf[:n]
		if firstChunk {
			firstChunk = false
			sniff := data
			if len(sniff) > 512 {
				sniff = sniff[:512]
			}
			mimeType = http.DetectContentType(sniff)
			if strings.HasPrefix(mimeType, "image/png") || strings.HasPrefix(mimeType, "image/jpeg") || strings.HasPrefix(mimeType, "image/webp") {
				if info.Size() > 5<<20 {
					return failResult("image exceeds 5 MiB limit", "too_large")
				}
				full, err := os.ReadFile(target)
				if err != nil {
					return failResult(err.Error(), "io_error")
				}
				dataURL := fmt.Sprintf("data:%s;base64,%s", strings.Split(mimeType, ";")[0], base64.StdEncoding.EncodeToString(full))
				return okResult(map[string]any{"path": target, "mime": mimeType, "bytes": len(full), "image_attached": true}, nil, []string{target}, []string{dataURL})
			}
		}
		if n > 0 {
			if bytes.IndexByte(data, 0) >= 0 {
				return okResult(map[string]any{"path": target, "binary": true, "mime": mimeType, "bytes": info.Size()}, nil, []string{target}, nil)
			}
			checkBuf := data
			if len(utf8Carry) > 0 {
				checkBuf = make([]byte, len(utf8Carry)+len(data))
				copy(checkBuf, utf8Carry)
				copy(checkBuf[len(utf8Carry):], data)
				utf8Carry = utf8Carry[:0]
			}
			if !utf8.Valid(checkBuf) {
				if !errors.Is(readErr, io.EOF) && len(checkBuf) > 0 {
					trim := 0
					for trim < 3 && trim < len(checkBuf) && !utf8.RuneStart(checkBuf[len(checkBuf)-1-trim]) {
						trim++
					}
					if trim < len(checkBuf) && utf8.RuneStart(checkBuf[len(checkBuf)-1-trim]) {
						trim++
					}
					if trim > 0 && trim <= 3 && !utf8.FullRune(checkBuf[len(checkBuf)-trim:]) && utf8.Valid(checkBuf[:len(checkBuf)-trim]) {
						utf8Carry = append(utf8Carry[:0], checkBuf[len(checkBuf)-trim:]...)
					} else {
						return okResult(map[string]any{"path": target, "binary": true, "mime": mimeType, "bytes": info.Size()}, nil, []string{target}, nil)
					}
				} else {
					return okResult(map[string]any{"path": target, "binary": true, "mime": mimeType, "bytes": info.Size()}, nil, []string{target}, nil)
				}
			}
			rem := data
			for len(rem) > 0 {
				idx := bytes.IndexByte(rem, '\n')
				var seg []byte
				hasNL := idx >= 0
				if hasNL {
					seg = rem[:idx]
					rem = rem[idx+1:]
				} else {
					seg = rem
					rem = nil
				}
				if lineNum >= start && (end <= 0 || lineNum <= end) && !truncated && len(seg) > 0 {
					appendBounded(seg)
				}
				if hasNL {
					if lineNum >= start && (end <= 0 || lineNum < end) && !truncated {
						appendBounded([]byte{'\n'})
					}
					lineNum++
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if len(utf8Carry) > 0 {
		return okResult(map[string]any{"path": target, "binary": true, "mime": mimeType, "bytes": info.Size()}, nil, []string{target}, nil)
	}

	totalLines := lineNum
	if end <= 0 || end > totalLines {
		end = totalLines
	}
	if start > totalLines {
		return okResult(map[string]any{"path": target, "total_lines": totalLines, "start_line": start, "end_line": end, "content": "", "truncated": false}, nil, []string{target}, nil)
	}
	content := selected.Bytes()
	if truncated && len(content) > 0 {
		cut := len(content) - 1
		for cut > 0 && len(content)-cut < 4 && !utf8.RuneStart(content[cut]) {
			cut--
		}
		if !utf8.FullRune(content[cut:]) {
			content = content[:cut]
		}
	}
	return okResult(map[string]any{
		"path":        target,
		"total_lines": totalLines,
		"start_line":  start,
		"end_line":    end,
		"content":     string(content),
		"truncated":   truncated,
	}, nil, []string{target}, nil)
}

func readRegularFileBounded(ctx context.Context, target string, maxBytes int) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("unsupported non-regular file type (%s)", info.Mode().Type())
	}
	if info.Size() > int64(maxBytes) {
		return nil, true, nil
	}
	f, err := os.Open(target)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var buf bytes.Buffer
	chunk := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		n, rerr := f.Read(chunk)
		if n > 0 {
			if buf.Len()+n > maxBytes {
				return nil, true, nil
			}
			buf.Write(chunk[:n])
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, false, rerr
		}
	}
	return buf.Bytes(), false, nil
}

func (l *Local) writeFile(ctx context.Context, rawArgs json.RawMessage) LocalResult {
	if err := ctx.Err(); err != nil {
		return failResult("write cancelled", "cancelled")
	}
	var args struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return failResult("invalid fs_write arguments", "invalid_request")
	}
	if args.Content == nil {
		return failResult("content is required", "invalid_request")
	}
	target, err := l.resolvePath(args.Path)
	if err != nil {
		return failResult(err.Error(), "invalid_request")
	}
	var oldText string
	if info, statErr := os.Stat(target); statErr == nil {
		if !info.Mode().IsRegular() {
			return failResult(fmt.Sprintf("unsupported non-regular file type (%s)", info.Mode().Type()), "invalid_request")
		}
		if prev, tooLarge, readErr := readRegularFileBounded(ctx, target, maxEditBytes); readErr != nil {
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				return failResult("write cancelled", "cancelled")
			}
		} else if !tooLarge && utf8.Valid(prev) {
			oldText = string(prev)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return failResult(statErr.Error(), "io_error")
	}
	if err := ctx.Err(); err != nil {
		return failResult("write cancelled", "cancelled")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return failResult(err.Error(), "io_error")
	}
	content := *args.Content
	if err := os.WriteFile(target, []byte(content), 0644); err != nil {
		return failResult(err.Error(), "io_error")
	}
	return okResult(
		map[string]any{"path": target, "bytes_written": len(content)},
		&FileDiff{Path: target, OldText: oldText, NewText: content},
		[]string{target},
		nil,
	)
}

func (l *Local) editFile(ctx context.Context, rawArgs json.RawMessage) LocalResult {
	if err := ctx.Err(); err != nil {
		return failResult("edit cancelled", "cancelled")
	}
	var args struct {
		Path       string  `json:"path"`
		OldText    string  `json:"old_text"`
		NewText    *string `json:"new_text"`
		ReplaceAll bool    `json:"replace_all"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return failResult("invalid fs_edit arguments", "invalid_request")
	}
	if args.OldText == "" {
		return failResult("old_text must be non-empty (use fs_write to create or replace an entire file)", "invalid_request")
	}
	if args.NewText == nil {
		return failResult("new_text is required", "invalid_request")
	}
	target, err := l.resolvePath(args.Path)
	if err != nil {
		return failResult(err.Error(), "invalid_request")
	}
	data, tooLarge, err := readRegularFileBounded(ctx, target, maxEditBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return failResult("edit cancelled", "cancelled")
		}
		if errors.Is(err, os.ErrNotExist) {
			return failResult(err.Error(), "not_found")
		}
		return failResult(err.Error(), "invalid_request")
	}
	if tooLarge {
		return failResult(fmt.Sprintf("file exceeds %d MiB edit limit", maxEditBytes>>20), "too_large")
	}
	oldContent := string(data)
	count := strings.Count(oldContent, args.OldText)
	if count == 0 {
		return failResult("old_text not found in file", "not_found")
	}
	if count > 1 && !args.ReplaceAll {
		return failResult(fmt.Sprintf("old_text matched %d times; provide more surrounding context or set replace_all=true", count), "ambiguous_match")
	}
	newText := *args.NewText
	var newContent string
	if args.ReplaceAll {
		newContent = strings.ReplaceAll(oldContent, args.OldText, newText)
	} else {
		newContent = strings.Replace(oldContent, args.OldText, newText, 1)
	}
	if err := ctx.Err(); err != nil {
		return failResult("edit cancelled", "cancelled")
	}
	if err := os.WriteFile(target, []byte(newContent), 0644); err != nil {
		return failResult(err.Error(), "io_error")
	}
	return okResult(
		map[string]any{"path": target, "replacements": count},
		&FileDiff{Path: target, OldText: oldContent, NewText: newContent},
		[]string{target},
		nil,
	)
}

func (l *Local) glob(ctx context.Context, rawArgs json.RawMessage) LocalResult {
	var args struct {
		Pattern string `json:"pattern"`
		Root    string `json:"root"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil || strings.TrimSpace(args.Pattern) == "" {
		return failResult("pattern is required", "invalid_request")
	}
	root := l.CWD
	if strings.TrimSpace(args.Root) != "" {
		var err error
		root, err = l.resolvePath(args.Root)
		if err != nil {
			return failResult(err.Error(), "invalid_request")
		}
	}
	limit := args.Limit
	if limit <= 0 || limit > maxGlobResults {
		limit = maxGlobResults
	}
	pattern := filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(args.Pattern), "./"))
	re, err := globToRegex(pattern)
	if err != nil {
		return failResult("invalid glob pattern: "+err.Error(), "invalid_request")
	}
	var matches []string
	truncated := false
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == ".next" || name == "vendor" && !strings.Contains(pattern, "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if re.MatchString(relSlash) || (!strings.Contains(pattern, "/") && re.MatchString(d.Name())) {
			if len(matches) >= limit {
				truncated = true
				return fs.SkipAll
			}
			matches = append(matches, relSlash)
		}
		return nil
	})
	sort.Strings(matches)
	return okResult(map[string]any{"root": root, "matches": matches, "truncated": truncated}, nil, nil, nil)
}

func globToRegex(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

type grepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func (l *Local) grep(ctx context.Context, rawArgs json.RawMessage) LocalResult {
	var args struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		Glob            string `json:"glob"`
		FixedStrings    bool   `json:"fixed_strings"`
		CaseInsensitive bool   `json:"case_insensitive"`
		Limit           int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil || args.Pattern == "" {
		return failResult("pattern is required", "invalid_request")
	}
	target := l.CWD
	if strings.TrimSpace(args.Path) != "" {
		var err error
		target, err = l.resolvePath(args.Path)
		if err != nil {
			return failResult(err.Error(), "invalid_request")
		}
	}
	limit := args.Limit
	if limit <= 0 || limit > maxGrepMatches {
		limit = maxGrepMatches
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		return l.grepWithRG(ctx, rg, target, args.Pattern, args.Glob, args.FixedStrings, args.CaseInsensitive, limit)
	}
	return l.grepFallback(ctx, target, args.Pattern, args.Glob, args.FixedStrings, args.CaseInsensitive, limit)
}

func (l *Local) grepWithRG(ctx context.Context, rg, target, pattern, glob string, fixed, ci bool, limit int) LocalResult {
	rgCtx, cancelRG := context.WithCancel(ctx)
	defer cancelRG()

	cmdArgs := []string{"--json", "--no-heading", "--max-count", fmt.Sprint(limit + 1)}
	if fixed {
		cmdArgs = append(cmdArgs, "-F")
	}
	if ci {
		cmdArgs = append(cmdArgs, "-i")
	}
	if strings.TrimSpace(glob) != "" {
		cmdArgs = append(cmdArgs, "-g", glob)
	}
	cmdArgs = append(cmdArgs, "--", pattern, target)
	cmd := exec.CommandContext(rgCtx, rg, cmdArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failResult(err.Error(), "exec_error")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return failResult(err.Error(), "exec_error")
	}
	matches := []grepMatch{}
	truncated := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Path       struct{ Text string } `json:"path"`
				LineNumber int                   `json:"line_number"`
				Lines      struct{ Text string } `json:"lines"`
			} `json:"data"`
		}
		if json.Unmarshal(scanner.Bytes(), &ev) != nil || ev.Type != "match" {
			continue
		}
		if len(matches) >= limit {
			truncated = true
			cancelRG()
			break
		}
		p := ev.Data.Path.Text
		if rel, err := filepath.Rel(l.CWD, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = filepath.ToSlash(rel)
		}
		text := strings.TrimRight(ev.Data.Lines.Text, "\r\n")
		if len(text) > 400 {
			text = text[:400] + "..."
		}
		matches = append(matches, grepMatch{Path: p, Line: ev.Data.LineNumber, Text: text})
	}
	scanErr := scanner.Err()
	_ = stdout.Close()
	_ = cmd.Wait()
	if !truncated && ctx.Err() == nil && scanErr != nil {
		return failResult("grep output scan failed: "+scanErr.Error(), "grep_error")
	}
	if len(matches) == 0 && !truncated && stderr.Len() > 0 {
		return failResult(strings.TrimSpace(stderr.String()), "grep_error")
	}
	return okResult(map[string]any{"matches": matches, "truncated": truncated}, nil, nil, nil)
}

func (l *Local) grepFallback(ctx context.Context, target, pattern, glob string, fixed, ci bool, limit int) LocalResult {
	expr := pattern
	if fixed {
		expr = regexp.QuoteMeta(pattern)
	}
	if ci {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return failResult("invalid regex: "+err.Error(), "invalid_request")
	}
	var globRe *regexp.Regexp
	if strings.TrimSpace(glob) != "" {
		globRe, err = globToRegex(filepath.ToSlash(strings.TrimSpace(glob)))
		if err != nil {
			return failResult("invalid glob: "+err.Error(), "invalid_request")
		}
	}
	matches := []grepMatch{}
	truncated := false
	_ = filepath.WalkDir(target, func(p string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			if d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			info, err := os.Stat(p)
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
		}
		rel, _ := filepath.Rel(l.CWD, p)
		relSlash := filepath.ToSlash(rel)
		if globRe != nil && !globRe.MatchString(relSlash) && !globRe.MatchString(d.Name()) {
			return nil
		}
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		const maxGrepFileBytes = 2 << 20
		var raw bytes.Buffer
		chunk := make([]byte, 32<<10)
		skipFile := false
		for {
			if ctx.Err() != nil {
				_ = f.Close()
				return ctx.Err()
			}
			n, rerr := f.Read(chunk)
			if n > 0 {
				if bytes.IndexByte(chunk[:n], 0) >= 0 || raw.Len()+n > maxGrepFileBytes {
					skipFile = true
					break
				}
				raw.Write(chunk[:n])
			}
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				skipFile = true
				break
			}
		}
		_ = f.Close()
		if skipFile {
			return nil
		}
		for idx, line := range strings.Split(raw.String(), "\n") {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if re.MatchString(line) {
				if len(matches) >= limit {
					truncated = true
					return fs.SkipAll
				}
				if len(line) > 400 {
					line = line[:400] + "..."
				}
				matches = append(matches, grepMatch{Path: relSlash, Line: idx + 1, Text: strings.TrimRight(line, "\r")})
			}
		}
		return nil
	})
	return okResult(map[string]any{"matches": matches, "truncated": truncated}, nil, nil, nil)
}

type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
	stream    OutputStreamer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stream != nil && len(p) > 0 {
		b.stream(string(p))
	}
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else {
		b.truncated = true
	}
	return len(p), nil
}

func (l *Local) runBash(ctx context.Context, rawArgs json.RawMessage, stream OutputStreamer) LocalResult {
	var args struct {
		Command        string `json:"command"`
		CWD            string `json:"cwd"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil || strings.TrimSpace(args.Command) == "" {
		return failResult("command is required", "invalid_request")
	}
	cwd := l.CWD
	if strings.TrimSpace(args.CWD) != "" {
		var err error
		cwd, err = l.resolvePath(args.CWD)
		if err != nil {
			return failResult(err.Error(), "invalid_request")
		}
	}
	timeout := defaultBashSec
	if args.TimeoutSeconds > 0 {
		timeout = min(args.TimeoutSeconds, maxBashSec)
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell, "-c", args.Command)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout := &boundedBuffer{limit: maxBashOut, stream: stream}
	stderr := &boundedBuffer{limit: maxBashOut, stream: stream}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return failResult(err.Error(), "exec_error")
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	timedOut := false
	cancelled := false
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		if ctx.Err() != nil {
			cancelled = true
		} else {
			timedOut = true
		}
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case waitErr = <-done:
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				waitErr = <-done
			}
		}
	}

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	payload := map[string]any{
		"cwd":       cwd,
		"exit_code": exitCode,
		"stdout":    stdout.buf.String(),
		"stderr":    stderr.buf.String(),
		"truncated": stdout.truncated || stderr.truncated,
		"timed_out": timedOut,
		"cancelled": cancelled,
	}
	raw, _ := json.Marshal(payload)
	return LocalResult{
		Output: raw,
		Failed: cancelled || timedOut,
	}
}
