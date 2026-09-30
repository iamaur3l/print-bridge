// Package config loads the agent's JSON configuration file.
//
// Configuration is deliberately forgiving: a missing file is created with defaults,
// and an unreadable file is backed up rather than allowed to stop the till printing.
// CLI flags always win over the file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultFilename is the file name used inside the agent's config directory.
const DefaultFilename = "config.json"

// ServerConfig covers the local listener.
type ServerConfig struct {
	Port        int    `json:"port"`
	TLSCertFile string `json:"tls_cert_file,omitempty"`
	TLSKeyFile  string `json:"tls_key_file,omitempty"`
}

// SecurityConfig covers browser access.
type SecurityConfig struct {
	// AllowedOrigins is additive: loopback origins are always accepted.
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	TrustedProxy   bool     `json:"trusted_proxy,omitempty"`
}

// QueueConfig covers the job queue limits.
type QueueConfig struct {
	MaxAttempts       int `json:"max_attempts"`
	MaxDepth          int `json:"max_depth"`
	MaxConcurrentJobs int `json:"max_concurrent_jobs"`
}

// UpdaterConfig covers self-update behaviour. Auto-update is off by default: an
// unattended till should be updated by an installer, not by itself.
type UpdaterConfig struct {
	AutoUpdate  bool   `json:"auto_update"`
	ManifestURL string `json:"manifest_url,omitempty"`
}

// RelayConfig configures the optional cloud relay. Relay credentials live here rather
// than in flags so that a service installation keeps them across a reboot; the file
// should therefore be readable only by administrators.
type RelayConfig struct {
	URL     string `json:"url,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
}

// Config is the whole file.
type Config struct {
	Server   ServerConfig   `json:"server"`
	Security SecurityConfig `json:"security"`
	Queue    QueueConfig    `json:"queue"`
	Updater  UpdaterConfig  `json:"updater"`
	Relay    RelayConfig    `json:"relay"`
}

// Source explains where a loaded configuration came from.
type Source string

const (
	// SourceDefault means no path was configured.
	SourceDefault Source = "built-in defaults"
	// SourceCreated means the file was missing and has been written with defaults.
	SourceCreated Source = "created with defaults"
	// SourceFile means the file was read successfully.
	SourceFile Source = "config file"
	// SourceRecovered means the file could not be parsed; it was backed up and
	// defaults were used.
	SourceRecovered Source = "recovered from an unreadable file"
)

// Result describes the outcome of a load.
type Result struct {
	Path       string
	Source     Source
	BackupPath string
	// Warning is a human-readable note for the agent log (may be empty).
	Warning string
	// Notes lists values that were clamped to sane ranges.
	Notes []string
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Server: ServerConfig{Port: 9567},
		Queue: QueueConfig{
			MaxAttempts:       5,
			MaxDepth:          5000,
			MaxConcurrentJobs: 4,
		},
		Updater: UpdaterConfig{AutoUpdate: false},
	}
}

// ApplyDefaults fills in unset values and clamps anything out of range, returning a
// note for each value it had to change. Clamping rather than failing keeps a broken
// config file from taking the till offline.
func (c *Config) ApplyDefaults() []string {
	notes := make([]string, 0, 4)

	defaults := Default()

	if c.Server.Port == 0 {
		c.Server.Port = defaults.Server.Port
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		notes = append(notes, fmt.Sprintf("server.port %d is out of range; using %d", c.Server.Port, defaults.Server.Port))
		c.Server.Port = defaults.Server.Port
	}

	if c.Queue.MaxAttempts <= 0 {
		c.Queue.MaxAttempts = defaults.Queue.MaxAttempts
	}
	if c.Queue.MaxAttempts > 100 {
		notes = append(notes, fmt.Sprintf("queue.max_attempts %d is excessive; using 100", c.Queue.MaxAttempts))
		c.Queue.MaxAttempts = 100
	}

	if c.Queue.MaxDepth <= 0 {
		c.Queue.MaxDepth = defaults.Queue.MaxDepth
	}

	if c.Queue.MaxConcurrentJobs <= 0 {
		c.Queue.MaxConcurrentJobs = defaults.Queue.MaxConcurrentJobs
	}
	if c.Queue.MaxConcurrentJobs > 32 {
		notes = append(notes, fmt.Sprintf("queue.max_concurrent_jobs %d is excessive; using 32", c.Queue.MaxConcurrentJobs))
		c.Queue.MaxConcurrentJobs = 32
	}

	// Drop blank origins rather than treating them as "allow anything".
	origins := make([]string, 0, len(c.Security.AllowedOrigins))
	for _, origin := range c.Security.AllowedOrigins {
		if trimmed := strings.TrimSpace(origin); trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	c.Security.AllowedOrigins = origins

	if (c.Server.TLSCertFile == "") != (c.Server.TLSKeyFile == "") {
		notes = append(notes, "server.tls_cert_file and server.tls_key_file must be set together; TLS is disabled")
		c.Server.TLSCertFile, c.Server.TLSKeyFile = "", ""
	}

	return notes
}

// Load reads the configuration file, always returning a usable configuration.
//
//   - a missing file is created with defaults,
//   - a UTF-8 BOM (as written by Notepad) is tolerated,
//   - an unparseable file is copied to config.corrupt.<timestamp>.json and defaults
//     are used, because losing printing is worse than losing settings.
func Load(path string) (Config, Result) {
	if strings.TrimSpace(path) == "" {
		cfg := Default()
		return cfg, Result{Source: SourceDefault}
	}

	result := Result{Path: path}

	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			result.Source = SourceRecovered
			result.Warning = fmt.Sprintf("could not read %s: %v; using defaults", path, err)
			cfg := Default()
			cfg.ApplyDefaults()
			return cfg, result
		}

		cfg := Default()
		cfg.ApplyDefaults()
		if saveErr := Save(path, cfg); saveErr != nil {
			result.Source = SourceDefault
			result.Warning = fmt.Sprintf("could not create %s: %v; using defaults", path, saveErr)
			return cfg, result
		}

		result.Source = SourceCreated
		return cfg, result
	}

	// Notepad and PowerShell both like to add a BOM; JSON does not.
	cleaned := strings.TrimPrefix(string(raw), "\uFEFF")

	var cfg Config
	if err := json.Unmarshal([]byte(cleaned), &cfg); err != nil {
		backupPath, backupErr := backupCorrupt(path, raw)
		result.Source = SourceRecovered
		result.BackupPath = backupPath
		result.Warning = fmt.Sprintf("%s is not valid JSON (%v)", path, err)
		if backupErr != nil {
			result.Warning += fmt.Sprintf("; and it could not be backed up: %v", backupErr)
		} else {
			result.Warning += fmt.Sprintf("; it was backed up to %s and defaults are in use", backupPath)
		}

		fallback := Default()
		fallback.ApplyDefaults()
		return fallback, result
	}

	result.Source = SourceFile
	result.Notes = cfg.ApplyDefaults()
	return cfg, result
}

// backupCorrupt copies an unreadable config aside instead of overwriting it.
func backupCorrupt(path string, raw []byte) (string, error) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	backupPath := filepath.Join(dir, fmt.Sprintf("%s.corrupt.%s%s", stem, time.Now().UTC().Format("20060102T150405"), ext))
	if err := os.WriteFile(backupPath, raw, 0o644); err != nil {
		return "", err
	}
	return backupPath, nil
}

// Save writes the configuration atomically, so a crash mid-write cannot leave a
// half-written file behind.
func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0o644); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// DefaultPath returns the configuration path inside a directory (usually the one
// holding the agent database).
func DefaultPath(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return DefaultFilename
	}
	return filepath.Join(dir, DefaultFilename)
}