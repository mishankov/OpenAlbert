package backup

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"

	"filippo.io/age"
)

type Config struct {
	Root            string `json:"root"`
	ComposeFile     string `json:"compose_file"`
	StateDir        string `json:"state_dir"`
	WorkDir         string `json:"work_dir"`
	Enabled         bool   `json:"enabled"`
	Time            string `json:"time"`
	Timezone        string `json:"timezone"`
	KeepDays        int    `json:"keep_days"`
	NoticeSeconds   int    `json:"notice_seconds"`
	Endpoint        string `json:"s3_endpoint"`
	Region          string `json:"s3_region"`
	Secure          bool   `json:"s3_secure"`
	Bucket          string `json:"s3_bucket"`
	Prefix          string `json:"s3_prefix"`
	Recipient       string `json:"age_recipient"`
	CredentialsFile string `json:"credentials_file,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	c := Config{ComposeFile: "compose.yaml", Enabled: true, Time: "03:00", Timezone: "Europe/Moscow", KeepDays: 14, NoticeSeconds: 60, Secure: true, Prefix: "openalbert/", WorkDir: "/var/lib/openalbert-backup"}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, errors.New("invalid backup config JSON")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("unexpected backup config JSON tail")
	}
	return c, c.Validate()
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (c *Config) Validate() error {
	if !filepath.IsAbs(c.Root) || !filepath.IsAbs(c.WorkDir) {
		return errors.New("root and work_dir must be absolute paths")
	}
	c.Root, c.WorkDir = filepath.Clean(c.Root), filepath.Clean(c.WorkDir)
	if c.StateDir == "" {
		c.StateDir = filepath.Join(c.Root, "container_data", "backup-state")
	}
	if c.StateDir != filepath.Join(c.Root, "container_data", "backup-state") {
		return errors.New("state_dir must be root/container_data/backup-state (the Compose read-only mount)")
	}
	if within(c.Root, c.WorkDir) || within(c.WorkDir, c.Root) {
		return errors.New("work_dir must be separate from the installation")
	}
	if !validComposeFile(c.ComposeFile) {
		return errors.New("compose_file must be compose.yaml or compose.local.yaml, optionally under deploy/")
	}
	if c.CredentialsFile != "" && !filepath.IsAbs(c.CredentialsFile) {
		return errors.New("credentials_file must be absolute")
	}
	if c.KeepDays < 1 || c.KeepDays > 3660 {
		return errors.New("keep_days must be between 1 and 3660")
	}
	if c.NoticeSeconds < 0 || c.NoticeSeconds > 3600 {
		return errors.New("notice_seconds must be between 0 and 3600")
	}
	if !regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`).MatchString(c.Time) {
		return errors.New("time must be HH:MM")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil || strings.ContainsAny(c.Timezone, "\r\n\t %") {
		return errors.New("invalid timezone")
	}
	if c.Endpoint == "" || strings.ContainsAny(c.Endpoint, "/?#@ \r\n") || c.Bucket == "" {
		return errors.New("S3 endpoint must be host[:port] and bucket must be set")
	}
	c.Prefix = strings.Trim(c.Prefix, "/") + "/"
	if c.Prefix == "/" || strings.Contains(c.Prefix, "..") || strings.ContainsAny(c.Prefix, "\r\n\\") {
		return errors.New("s3_prefix must be a nonempty dedicated prefix")
	}
	if _, err := age.ParseX25519Recipient(c.Recipient); err != nil {
		return errors.New("age_recipient must be an age X25519 public key")
	}
	return nil
}

func (c Config) NextRun(now time.Time) time.Time {
	loc, _ := time.LoadLocation(c.Timezone)
	clock, _ := time.Parse("15:04", c.Time)
	// Walk actual minutes so a nonexistent wall-clock time at a DST transition
	// is skipped, rather than normalized to an hour the timer will never use.
	next := now.Truncate(time.Minute).Add(time.Minute)
	for minute := 0; minute < 3*24*60; minute++ {
		local := next.In(loc)
		if local.Hour() == clock.Hour() && local.Minute() == clock.Minute() {
			return local
		}
		next = next.Add(time.Minute)
	}
	return time.Time{}
}

func validComposeFile(name string) bool {
	return name == "compose.yaml" || name == "compose.local.yaml" || name == "deploy/compose.yaml" || name == "deploy/compose.local.yaml"
}
