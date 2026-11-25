package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mishankov/OpenAlbert/apps/cli/internal/serverclient"
)

// Skill represents one discovered local or OpenAlbert server skill.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"` // workspace, user, server
	Path        string `json:"path,omitempty"`
	Directory   string `json:"directory,omitempty"`
}

// Invocation is returned by skill_invoke inside Codemode.
type Invocation struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Directory   string `json:"directory,omitempty"`
	Path        string `json:"path,omitempty"`
	Args        string `json:"args,omitempty"`
	Content     string `json:"content"`
}

// RemoteReader loads an OpenAlbert server skill's full content on demand.
type RemoteReader func(ctx context.Context, name string) (string, error)

// Registry discovers and invokes workspace, user, and OpenAlbert server skills.
type Registry struct {
	skills       map[string]Skill
	order        []string
	remoteReader RemoteReader
}

// Discover scans workspace (.agents/skills, .claude/skills), user home
// (~/.agents/skills, ~/.claude/skills), and OpenAlbert server skills with
// precedence: workspace > user > server.
func Discover(cwd string, homeDir string, serverSkills []serverclient.ServerSkill, reader RemoteReader) *Registry {
	r := &Registry{
		skills:       map[string]Skill{},
		remoteReader: reader,
	}
	// Lowest priority first so higher priority layers cleanly overwrite.
	for _, s := range serverSkills {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			continue
		}
		r.skills[name] = Skill{
			Name:        name,
			Description: strings.TrimSpace(s.Description),
			Source:      "server",
		}
	}
	if homeDir == "" {
		homeDir, _ = os.UserHomeDir()
	}
	if homeDir != "" {
		r.scanDir(filepath.Join(homeDir, ".claude", "skills"), "user")
		r.scanDir(filepath.Join(homeDir, ".agents", "skills"), "user")
	}
	if cwd != "" {
		roots := workspaceRoots(cwd)
		for _, root := range roots {
			r.scanDir(filepath.Join(root, ".claude", "skills"), "workspace")
			r.scanDir(filepath.Join(root, ".agents", "skills"), "workspace")
		}
	}
	r.order = make([]string, 0, len(r.skills))
	for name := range r.skills {
		r.order = append(r.order, name)
	}
	sort.Strings(r.order)
	return r
}

func workspaceRoots(cwd string) []string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return []string{cwd}
	}
	// Walk up to git root (if any) so skills at repo root are discovered even
	// when cwd is a subdirectory. Return outer-to-inner so closer skills win.
	var chain []string
	cur := abs
	for {
		chain = append(chain, cur)
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

func (r *Registry) scanDir(dir, source string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		skillDir := filepath.Join(dir, entry.Name())
		skillFile := filepath.Join(skillDir, "SKILL.md")
		data, err := os.ReadFile(skillFile)
		if err != nil {
			continue
		}
		name, desc := parseFrontmatter(string(data))
		if name == "" {
			name = entry.Name()
		}
		r.skills[name] = Skill{
			Name:        name,
			Description: desc,
			Source:      source,
			Path:        skillFile,
			Directory:   skillDir,
		}
	}
}

func parseFrontmatter(content string) (name, description string) {
	trimmed := strings.TrimPrefix(content, "\uFEFF")
	if !strings.HasPrefix(trimmed, "---") {
		return "", ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", ""
	}
	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if strings.TrimSpace(line) == "---" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		switch k {
		case "name":
			name = v
		case "description":
			description = v
		}
	}
	return name, description
}

func (r *Registry) List() []Skill {
	out := make([]Skill, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.skills[name])
	}
	return out
}

func (r *Registry) Get(name string) (Skill, bool) {
	s, ok := r.skills[name]
	return s, ok
}

// Invoke loads the full instructions for a local or server skill.
func (r *Registry) Invoke(ctx context.Context, name, args string) (Invocation, error) {
	name = strings.TrimSpace(strings.TrimPrefix(name, "/"))
	if name == "" {
		return Invocation{}, errors.New("skill name is required")
	}
	s, ok := r.skills[name]
	if !ok {
		return Invocation{}, fmt.Errorf("skill %q not found", name)
	}
	var content string
	if s.Source == "server" {
		if r.remoteReader == nil {
			return Invocation{}, fmt.Errorf("server skill %q cannot be loaded (remote reader unavailable)", name)
		}
		var err error
		content, err = r.remoteReader(ctx, name)
		if err != nil {
			return Invocation{}, err
		}
	} else {
		data, err := os.ReadFile(s.Path)
		if err != nil {
			return Invocation{}, fmt.Errorf("read skill %q: %w", name, err)
		}
		content = string(data)
	}
	return Invocation{
		Name:        s.Name,
		Description: s.Description,
		Source:      s.Source,
		Directory:   s.Directory,
		Path:        s.Path,
		Args:        strings.TrimSpace(args),
		Content:     content,
	}, nil
}

// ToolDefinition returns the Codemode tool schema for skill_invoke.
func ToolDefinition() serverclient.Tool {
	return serverclient.Tool{
		Name:        "skill_invoke",
		Description: "Load and invoke a discovered skill (workspace, user, or OpenAlbert server) by exact name. Returns the full SKILL.md instructions and its directory path for reading references or scripts.",
		Parameters:  json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string","description":"Exact skill name from the available skills list."},"args":{"type":"string","description":"Optional arguments or context passed to the skill."}},"required":["name"]}`),
	}
}
