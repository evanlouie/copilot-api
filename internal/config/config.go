package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/evanlouie/copilot-api/internal/safepath"
)

const AppName = "copilot-api"

const (
	DefaultAddr                        = "127.0.0.1:8080"
	DefaultModelsCacheTTL              = 10 * time.Minute
	DefaultToolCallTTL                 = 5 * time.Minute
	DefaultMaxRequestBodyBytes         = 0
	DefaultMaxTurnOutputBytes          = 32 << 20
	DefaultRetentionMaxAge             = 30 * 24 * time.Hour
	DefaultRetentionMaxResponses       = 10_000
	DefaultRetentionMaxBytes     int64 = 2 << 30
	DefaultWebSocketIdleTimeout        = 2 * time.Minute
	DefaultWebSocketPingInterval       = 30 * time.Second
	DefaultReasoningEmission           = "both"
)

// Reasoning emission policy values control which de-facto-standard reasoning
// fields the OpenAI-compatible surfaces expose. "both" maximizes client
// compatibility; the narrower values let operators silence clients that render
// reasoning twice.
const (
	ReasoningEmissionBoth             = "both"
	ReasoningEmissionReasoning        = "reasoning"
	ReasoningEmissionReasoningContent = "reasoning_content"
	ReasoningEmissionOff              = "off"
)

type Config struct {
	Addr                   string
	APIKey                 string
	GitHubToken            string
	CLIPath                string
	DefaultReasoningEffort string
	ModelsCacheTTL         time.Duration
	ToolCallTTL            time.Duration
	RequestTimeout         time.Duration
	MaxRequestBodyBytes    int64
	MaxTurnOutputBytes     int64
	RetentionMaxAge        time.Duration
	RetentionMaxResponses  int64
	RetentionMaxBytes      int64
	WebSocketIdleTimeout   time.Duration
	WebSocketMaxLifetime   time.Duration
	WebSocketPingInterval  time.Duration
	DataDir                string
	StateDir               string
	CacheDir               string
	ConfigDir              string
	StrictCompat           bool
	ReasoningEmission      string
	LogContent             bool
	LogLevel               slog.Level
}

func Load() (Config, error) {
	var err error
	cfg := Config{
		Addr:                   getenv("COPILOT_API_ADDR", DefaultAddr),
		APIKey:                 os.Getenv("COPILOT_API_KEY"),
		GitHubToken:            os.Getenv("GITHUB_TOKEN"),
		CLIPath:                os.Getenv("COPILOT_CLI_PATH"),
		DefaultReasoningEffort: strings.ToLower(strings.TrimSpace(os.Getenv("COPILOT_DEFAULT_REASONING_EFFORT"))),
		StrictCompat:           false,
		ModelsCacheTTL:         DefaultModelsCacheTTL,
		ToolCallTTL:            DefaultToolCallTTL,
		MaxRequestBodyBytes:    DefaultMaxRequestBodyBytes,
		MaxTurnOutputBytes:     DefaultMaxTurnOutputBytes,
		RetentionMaxAge:        DefaultRetentionMaxAge,
		RetentionMaxResponses:  DefaultRetentionMaxResponses,
		RetentionMaxBytes:      DefaultRetentionMaxBytes,
		WebSocketIdleTimeout:   DefaultWebSocketIdleTimeout,
		WebSocketPingInterval:  DefaultWebSocketPingInterval,
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
	if cfg.MaxRequestBodyBytes, err = parseBytesEnv("COPILOT_MAX_REQUEST_BODY_BYTES", DefaultMaxRequestBodyBytes); err != nil {
		return Config{}, err
	}
	if cfg.MaxTurnOutputBytes, err = parsePositiveBytesEnv("COPILOT_MAX_TURN_OUTPUT_BYTES", DefaultMaxTurnOutputBytes); err != nil {
		return Config{}, err
	}
	if cfg.RetentionMaxAge, err = parseDurationEnv("COPILOT_RETENTION_MAX_AGE", DefaultRetentionMaxAge); err != nil {
		return Config{}, err
	}
	if cfg.RetentionMaxResponses, err = parseInt64Env("COPILOT_RETENTION_MAX_RESPONSES", DefaultRetentionMaxResponses); err != nil {
		return Config{}, err
	}
	if cfg.RetentionMaxBytes, err = parseBytesEnv("COPILOT_RETENTION_MAX_BYTES", DefaultRetentionMaxBytes); err != nil {
		return Config{}, err
	}
	if cfg.WebSocketIdleTimeout, err = parseDurationEnv("COPILOT_WEBSOCKET_IDLE_TIMEOUT", DefaultWebSocketIdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.WebSocketMaxLifetime, err = parseDurationEnv("COPILOT_WEBSOCKET_MAX_LIFETIME", 0); err != nil {
		return Config{}, err
	}
	if cfg.WebSocketPingInterval, err = parseDurationEnv("COPILOT_WEBSOCKET_PING_INTERVAL", DefaultWebSocketPingInterval); err != nil {
		return Config{}, err
	}
	if cfg.StrictCompat, err = parseBoolEnv("COPILOT_STRICT_COMPAT", false); err != nil {
		return Config{}, err
	}
	if cfg.ReasoningEmission, err = parseReasoningEmissionEnv("COPILOT_REASONING_EMISSION", DefaultReasoningEmission); err != nil {
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
	for _, item := range []struct {
		name string
		dir  *string
	}{
		{"COPILOT_API_DATA_DIR", &cfg.DataDir},
		{"COPILOT_API_STATE_DIR", &cfg.StateDir},
		{"COPILOT_API_CACHE_DIR", &cfg.CacheDir},
		{"COPILOT_API_CONFIG_DIR", &cfg.ConfigDir},
	} {
		canonical, err := canonicalDirectory(*item.dir)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", item.name, err)
		}
		*item.dir = canonical
	}
	return cfg, nil
}

func canonicalDirectory(dir string) (string, error) {
	return safepath.Resolve(dir)
}

func (c Config) ValidateDirs() error {
	return validateAppDirs([]string{c.DataDir, c.StateDir, c.CacheDir, c.ConfigDir})
}

func (c Config) EnsureConfigDir() error {
	if err := c.ValidateDirs(); err != nil {
		return err
	}
	return ensureConfigDir(c.ConfigDir)
}

func ensureConfigDir(dir string) error {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	} else if err != nil {
		return fmt.Errorf("inspect config directory %s: %w", dir, err)
	} else if !info.IsDir() {
		return fmt.Errorf("config path is not a directory: %s", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure %s: %w", dir, err)
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
		if d < 0 {
			return 0, fmt.Errorf("%s must be non-negative", key)
		}
		return d, nil
	}
	seconds, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration (for example 5m) or seconds: %w", key, err)
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return 0, fmt.Errorf("%s must be finite, non-negative, and within the supported duration range", key)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func parseBytesEnv(key string, def int64) (int64, error) {
	return parseInt64Env(key, def)
}

func parsePositiveBytesEnv(key string, def int64) (int64, error) {
	n, err := parseInt64Env(key, def)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("%s must be a positive integer byte count", key)
	}
	return n, nil
}

func parseInt64Env(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return n, nil
}

func validateAppDirs(dirs []string) error {
	_, err := safepath.ValidateApplicationRoots(dirs)
	return err
}

func parseReasoningEmissionEnv(key, def string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def, nil
	}
	switch v {
	case ReasoningEmissionBoth, ReasoningEmissionReasoning, ReasoningEmissionReasoningContent, ReasoningEmissionOff:
		return v, nil
	default:
		return "", fmt.Errorf("%s must be one of both, reasoning, reasoning_content, or off", key)
	}
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
