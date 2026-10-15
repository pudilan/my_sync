// Package config loads, validates and persists the my_sync configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Environment variables that override values from the config file.
const (
	EnvPassword   = "MYSYNC_PASSWORD"
	EnvUIUser     = "MYSYNC_UI_USER"
	EnvUIPassword = "MYSYNC_UI_PASSWORD"
	EnvListen     = "MYSYNC_LISTEN"
)

// Supported encryption and compression methods.
const (
	EncryptionAES256    = "aes256"
	EncryptionZipCrypto = "zipcrypto"

	CompressionDeflate = "deflate"
	CompressionStore   = "store"
)

// Source is one directory to back up. Name becomes the top-level folder
// inside the archives, so files of different sources never collide.
type Source struct {
	Name string `yaml:"name" json:"name"`
	Path string `yaml:"path" json:"path"`
}

// UIConfig holds the optional HTTP basic auth credentials of the web UI.
type UIConfig struct {
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"-"`
}

// Config is the full application configuration.
type Config struct {
	Listen      string   `yaml:"listen" json:"listen"`
	Dest        string   `yaml:"dest" json:"dest"`
	Prefix      string   `yaml:"prefix" json:"prefix"`
	Sources     []Source `yaml:"sources" json:"sources"`
	Excludes    []string `yaml:"excludes" json:"excludes"`
	Password    string   `yaml:"password" json:"-"`
	Encryption  string   `yaml:"encryption" json:"encryption"`
	Compression string   `yaml:"compression" json:"compression"`
	// VolumeSize is a human readable size such as "500MB" or "2GB".
	// Empty or "0" disables splitting.
	VolumeSize string `yaml:"volume_size" json:"volume_size"`
	// MaxBackupSize caps the archive bytes written by a single run, e.g.
	// "50GB". Changes that do not fit are left for the next run. Empty or
	// "0" means unlimited.
	MaxBackupSize string `yaml:"max_backup_size" json:"max_backup_size"`
	// Schedule is either empty (disabled), a Go duration ("6h", "30m")
	// meaning "every interval", or a daily time "HH:MM".
	Schedule string   `yaml:"schedule" json:"schedule"`
	UI       UIConfig `yaml:"ui" json:"ui"`
}

// Default returns a configuration with sensible defaults.
func Default() Config {
	return Config{
		Listen:      ":8080",
		Dest:        "/backups",
		Prefix:      "backup",
		Encryption:  EncryptionAES256,
		Compression: CompressionDeflate,
		VolumeSize:  "1GB",
		Excludes:    []string{".DS_Store", ".localized", "Thumbs.db", "*.tmp", "~$*"},
	}
}

var nameRe = regexp.MustCompile(`^[\p{L}\p{N}_.\-]+$`)

// Normalize fills defaults and cleans paths. It does not touch the filesystem.
func (c *Config) Normalize() {
	d := Default()
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.Prefix == "" {
		c.Prefix = d.Prefix
	}
	c.Encryption = strings.ToLower(strings.TrimSpace(c.Encryption))
	if c.Encryption == "" {
		c.Encryption = d.Encryption
	}
	c.Compression = strings.ToLower(strings.TrimSpace(c.Compression))
	if c.Compression == "" {
		c.Compression = d.Compression
	}
	c.Dest = strings.TrimSpace(c.Dest)
	if c.Dest != "" {
		c.Dest = filepath.Clean(c.Dest)
	}
	for i := range c.Sources {
		c.Sources[i].Path = strings.TrimSpace(c.Sources[i].Path)
		if c.Sources[i].Path != "" {
			c.Sources[i].Path = filepath.Clean(c.Sources[i].Path)
		}
		c.Sources[i].Name = strings.TrimSpace(c.Sources[i].Name)
		if c.Sources[i].Name == "" && c.Sources[i].Path != "" {
			c.Sources[i].Name = filepath.Base(c.Sources[i].Path)
		}
	}
	excludes := c.Excludes[:0]
	for _, e := range c.Excludes {
		if e = strings.TrimSpace(e); e != "" {
			excludes = append(excludes, e)
		}
	}
	c.Excludes = excludes
}

// Validate checks the configuration for errors that would make a backup
// impossible. Missing sources are allowed so that a fresh install can start
// the web UI and be configured from there.
func (c *Config) Validate() error {
	var errs []string
	if c.Dest == "" {
		errs = append(errs, "dest 不能为空")
	}
	if !nameRe.MatchString(c.Prefix) {
		errs = append(errs, "prefix 只能包含字母、数字、_ . -")
	}
	switch c.Encryption {
	case EncryptionAES256, EncryptionZipCrypto:
	default:
		errs = append(errs, fmt.Sprintf("encryption 不支持: %q (可选 aes256 / zipcrypto)", c.Encryption))
	}
	switch c.Compression {
	case CompressionDeflate, CompressionStore:
	default:
		errs = append(errs, fmt.Sprintf("compression 不支持: %q (可选 deflate / store)", c.Compression))
	}
	if _, err := ParseSize(c.VolumeSize); err != nil {
		errs = append(errs, "volume_size: "+err.Error())
	}
	if _, err := ParseSize(c.MaxBackupSize); err != nil {
		errs = append(errs, "max_backup_size: "+err.Error())
	}
	if _, err := ParseSchedule(c.Schedule); err != nil {
		errs = append(errs, "schedule: "+err.Error())
	}
	seen := map[string]bool{}
	for i, s := range c.Sources {
		if s.Path == "" {
			errs = append(errs, fmt.Sprintf("sources[%d].path 不能为空", i))
		}
		if !nameRe.MatchString(s.Name) {
			errs = append(errs, fmt.Sprintf("sources[%d].name %q 非法 (只能包含字母、数字、_ . -)", i, s.Name))
		}
		if seen[s.Name] {
			errs = append(errs, fmt.Sprintf("sources[%d].name %q 重复", i, s.Name))
		}
		seen[s.Name] = true
	}
	for _, e := range c.Excludes {
		if _, err := filepath.Match(e, "x"); err != nil {
			errs = append(errs, fmt.Sprintf("excludes %q: %v", e, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// EnvSet reports the value of an environment variable that is set and not
// empty. Empty values are ignored so that docker-compose entries such as
// MYSYNC_PASSWORD: ${MYSYNC_PASSWORD:-} do not wipe values from the file.
func EnvSet(name string) (string, bool) {
	v := os.Getenv(name)
	return v, v != ""
}

// ApplyEnv overrides secrets and listen address from environment variables.
func (c *Config) ApplyEnv() {
	if v, ok := EnvSet(EnvPassword); ok {
		c.Password = v
	}
	if v, ok := EnvSet(EnvUIUser); ok {
		c.UI.Username = v
	}
	if v, ok := EnvSet(EnvUIPassword); ok {
		c.UI.Password = v
	}
	if v, ok := EnvSet(EnvListen); ok {
		c.Listen = v
	}
}

// VolumeBytes returns the parsed volume size (0 = unlimited).
func (c *Config) VolumeBytes() int64 {
	n, _ := ParseSize(c.VolumeSize)
	return n
}

// MaxBackupBytes returns the parsed per-run size limit (0 = unlimited).
func (c *Config) MaxBackupBytes() int64 {
	n, _ := ParseSize(c.MaxBackupSize)
	return n
}

// Clone returns a deep copy.
func (c Config) Clone() Config {
	c.Sources = append([]Source(nil), c.Sources...)
	c.Excludes = append([]string(nil), c.Excludes...)
	return c
}

var sizeRe = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*([kmgt]?)(i?b?)\s*$`)

// ParseSize parses sizes like "500MB", "1.5G", "1024", "2GiB" (1024 based).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("无法解析大小 %q (示例: 500MB, 2GB)", s)
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := map[string]float64{"": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40}[strings.ToLower(m[2])]
	return int64(f * mult), nil
}

// FormatSize renders a byte count in a human readable form.
func FormatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

// Schedule describes when automatic backups run.
type Schedule struct {
	Every   time.Duration // > 0: run at a fixed interval
	DailyAt time.Duration // offset since midnight, valid when Daily is true
	Daily   bool
}

// Enabled reports whether any automatic schedule is configured.
func (s Schedule) Enabled() bool { return s.Every > 0 || s.Daily }

// Next returns the next run time after now.
func (s Schedule) Next(now time.Time) time.Time {
	switch {
	case s.Daily:
		y, m, d := now.Date()
		t := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).Add(s.DailyAt)
		if !t.After(now) {
			t = time.Date(y, m, d+1, 0, 0, 0, 0, now.Location()).Add(s.DailyAt)
		}
		return t
	case s.Every > 0:
		return now.Add(s.Every)
	}
	return time.Time{}
}

var dailyRe = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):([0-5][0-9])$`)

// ParseSchedule parses "", "6h" / "@every 6h" or "02:30" / "daily 02:30".
func ParseSchedule(s string) (Schedule, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimPrefix(s, "@every"))
	s = strings.TrimSpace(strings.TrimPrefix(s, "daily"))
	if s == "" {
		return Schedule{}, nil
	}
	if m := dailyRe.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		return Schedule{Daily: true, DailyAt: time.Duration(h)*time.Hour + time.Duration(mi)*time.Minute}, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return Schedule{}, fmt.Errorf("无法解析 %q (示例: 6h, 30m, 02:30)", s)
	}
	if d < time.Minute {
		return Schedule{}, fmt.Errorf("间隔不能小于 1 分钟")
	}
	return Schedule{Every: d}, nil
}

// Store guards a configuration that can be read and updated concurrently
// and persisted back to its YAML file.
type Store struct {
	mu   sync.RWMutex
	path string
	file Config // as stored on disk, without env overrides
	cfg  Config // effective config, with env overrides
}

// Load reads the config file at path. A missing file yields defaults and is
// created so that the UI can edit it later.
func Load(path string) (*Store, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		cfg = Config{}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := writeFile(path, cfg); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置无效: %w", err)
	}
	s := &Store{path: path}
	s.set(cfg)
	return s, nil
}

func (s *Store) set(file Config) {
	s.file = file.Clone()
	eff := file.Clone()
	eff.ApplyEnv()
	s.cfg = eff
}

// Path returns the config file location.
func (s *Store) Path() string { return s.path }

// Get returns a copy of the effective configuration.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Clone()
}

// File returns a copy of the configuration as stored on disk.
func (s *Store) File() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file.Clone()
}

// Update validates and persists a new on-disk configuration.
func (s *Store) Update(c Config) error {
	c.Normalize()
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeFile(s.path, c); err != nil {
		return err
	}
	s.set(c)
	return nil
}

// PasswordFromEnv reports whether the archive password comes from the env.
func PasswordFromEnv() bool {
	_, ok := EnvSet(EnvPassword)
	return ok
}

const fileHeader = `# my_sync configuration
# Secrets can also be supplied via env: MYSYNC_PASSWORD, MYSYNC_UI_USER, MYSYNC_UI_PASSWORD
`

func writeFile(path string, c Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(fileHeader), data...), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
