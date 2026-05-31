package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const AppName = "copilot-api"

const (
	DefaultAddr           = "127.0.0.1:8080"
	DefaultModelsCacheTTL = 10 * time.Minute
	DefaultToolCallTTL    = 5 * time.Minute
)

type Config struct {
	Addr                string
	APIKey              string
	GitHubToken         string
	CLIPath             string
	ModelsCacheTTL      time.Duration
	ToolCallTTL         time.Duration
	RequestTimeout      time.Duration
	MaxRequestBodyBytes int64
	DataDir             string
	StateDir            string
	CacheDir            string
	ConfigDir           string
	StrictCompat        bool
	LogContent          bool
	LogLevel            slog.Level
}

func Load() (Config, error) {
	var err error
	cfg := Config{
		Addr:           getenv("COPILOT_API_ADDR", DefaultAddr),
		APIKey:         os.Getenv("COPILOT_API_KEY"),
		GitHubToken:    os.Getenv("GITHUB_TOKEN"),
		CLIPath:        os.Getenv("COPILOT_CLI_PATH"),
		StrictCompat:   false,
		ModelsCacheTTL: DefaultModelsCacheTTL,
		ToolCallTTL:    DefaultToolCallTTL,
	}

	if cfg.ModelsCacheTTL, err = parseDurationEnv("COPILOT_MODELS_CACHE_TTL", DefaultModelsCacheTTL); err != nil {
		return Config{}, err
	}
	if cfg.ToolCallTTL, err = parseDurationEnv("COPILOT_TOOL_CALL_TTL", DefaultToolCallTTL); err != nil {
		return Config{}, err
	}
	if cfg.RequestTimeout, err = parseDurationEnv("COPILOT_REQUEST_TIMEOUT", 0); err != nil {
		return Config{}, err
	}
	if cfg.MaxRequestBodyBytes, err = parseBytesEnv("COPILOT_MAX_REQUEST_BODY_BYTES", 0); err != nil {
		return Config{}, err
	}
	if cfg.StrictCompat, err = parseBoolEnv("COPILOT_STRICT_COMPAT", false); err != nil {
		return Config{}, err
	}
	if cfg.LogContent, err = parseBoolEnv("COPILOT_LOG_CONTENT", false); err != nil {
		return Config{}, err
	}
	level := strings.ToLower(strings.TrimSpace(getenv("COPILOT_LOG_LEVEL", "info")))
	switch level {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "info", "":
		cfg.LogLevel = slog.LevelInfo
	case "warn", "warning":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		return Config{}, fmt.Errorf("COPILOT_LOG_LEVEL must be debug, info, warn, or error")
	}

	cfg.DataDir = getenv("COPILOT_API_DATA_DIR", filepath.Join(xdgDataHome(), AppName))
	cfg.StateDir = getenv("COPILOT_API_STATE_DIR", filepath.Join(xdgStateHome(), AppName))
	cfg.CacheDir = getenv("COPILOT_API_CACHE_DIR", filepath.Join(xdgCacheHome(), AppName))
	cfg.ConfigDir = getenv("COPILOT_API_CONFIG_DIR", filepath.Join(xdgConfigHome(), AppName))
	return cfg, nil
}

func (c Config) EnsureDirs() error {
	for _, dir := range []string{c.DataDir, c.StateDir, c.CacheDir, c.ConfigDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		_ = os.Chmod(dir, 0o700)
	}
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseDurationEnv(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	if v == "0" {
		return 0, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d, nil
	}
	seconds, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration (for example 5m) or seconds: %w", key, err)
	}
	if seconds < 0 {
		return 0, fmt.Errorf("%s must be non-negative", key)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func parseBytesEnv(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer byte count", key)
	}
	return n, nil
}

func parseBoolEnv(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return b, nil
}

func xdgDataHome() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share")
	}
	return filepath.Join(os.TempDir(), "xdg-data")
}

func xdgStateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state")
	}
	return filepath.Join(os.TempDir(), "xdg-state")
}

func xdgCacheHome() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return v
	}
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "xdg-cache")
}

func xdgConfigHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	if d, err := os.UserConfigDir(); err == nil && d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config")
	}
	return filepath.Join(os.TempDir(), "xdg-config")
}
