package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const ControllerService = "backup"

type mount struct{ Type, Source, Destination string }

// NewRunner derives the host path and project from the controller's actual Docker
// mounts/labels. The host may use any installation path or Compose project name.
func NewRunner(ctx context.Context, c Config) (*Runner, error) {
	r := &Runner{Config: c}
	if os.Getenv("BACKUP_CONTAINER") != "true" {
		return r, nil
	}
	name, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	raw, err := RunCommand(ctx, "docker", "inspect", name, "--format", "{{json .Mounts}}")
	if err != nil {
		return nil, err
	}
	var mounts []mount
	if json.Unmarshal(raw, &mounts) != nil {
		return nil, errors.New("cannot inspect backup container mounts")
	}
	for _, m := range mounts {
		if m.Type == "bind" && m.Destination == c.Root {
			r.HostRoot = m.Source
		}
	}
	if !filepath.IsAbs(r.HostRoot) {
		return nil, errors.New("backup container requires a bind mount at the configured root")
	}
	raw, err = RunCommand(ctx, "docker", "inspect", name, "--format", `{{index .Config.Labels "com.docker.compose.project"}}`)
	if err != nil {
		return nil, err
	}
	r.ProjectName = strings.TrimSpace(string(raw))
	if r.ProjectName == "" || r.ProjectName == "<no value>" {
		return nil, errors.New("backup container must belong to a Compose project")
	}
	return r, nil
}

func withoutController(services []string) []string {
	out := make([]string, 0, len(services))
	for _, name := range services {
		if name != ControllerService {
			out = append(out, name)
		}
	}
	return out
}
