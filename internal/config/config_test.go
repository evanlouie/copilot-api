package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateAppDirsRejectsProtectedAncestorsAndOverlap(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := validateAppDirs([]string{filepath.Dir(cwd), filepath.Join(root, "state")}); err == nil {
		t.Fatal("expected working-directory ancestor rejection")
	}
	if err := validateAppDirs([]string{filepath.Join(root, "data"), filepath.Join(root, "data", "nested")}); err == nil {
		t.Fatal("expected overlapping directory rejection")
	}
}

func TestEnsureConfigDirSecuresCustomRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custom-config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureConfigDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("config permissions = %o", info.Mode().Perm())
	}
}

func TestStrictCompatDefaultsFalse(t *testing.T) {
	setLoadEnv(t)
	t.Setenv("COPILOT_STRICT_COMPAT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StrictCompat {
		t.Fatal("StrictCompat default = true, want false")
	}
}

func TestStrictCompatEnvOverrideTrue(t *testing.T) {
	setLoadEnv(t)
	t.Setenv("COPILOT_STRICT_COMPAT", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.StrictCompat {
		t.Fatal("StrictCompat override = false, want true")
	}
}

func TestDefaultReasoningEffortEnvOverride(t *testing.T) {
	setLoadEnv(t)
	t.Setenv("COPILOT_DEFAULT_REASONING_EFFORT", " HIGH ")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultReasoningEffort != "high" {
		t.Fatalf("DefaultReasoningEffort = %q, want high", cfg.DefaultReasoningEffort)
	}
}

func TestDurationEnvironmentValidation(t *testing.T) {
	for _, value := range []string{"-1s", "-1", "NaN", "+Inf", "1e100"} {
		t.Run(value, func(t *testing.T) {
			setLoadEnv(t)
			t.Setenv("COPILOT_REQUEST_TIMEOUT", value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted invalid duration %q", value)
			}
		})
	}
	for _, value := range []string{"0", "1.5", "250ms"} {
		t.Run("valid_"+value, func(t *testing.T) {
			setLoadEnv(t)
			t.Setenv("COPILOT_REQUEST_TIMEOUT", value)
			if _, err := Load(); err != nil {
				t.Fatalf("Load rejected duration %q: %v", value, err)
			}
		})
	}
}

func TestReasoningEmissionDefaultsBoth(t *testing.T) {
	setLoadEnv(t)
	t.Setenv("COPILOT_REASONING_EMISSION", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReasoningEmission != ReasoningEmissionBoth {
		t.Fatalf("ReasoningEmission default = %q, want both", cfg.ReasoningEmission)
	}
}

func TestReasoningEmissionEnvOverride(t *testing.T) {
	setLoadEnv(t)
	t.Setenv("COPILOT_REASONING_EMISSION", " Reasoning_Content ")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReasoningEmission != ReasoningEmissionReasoningContent {
		t.Fatalf("ReasoningEmission = %q, want reasoning_content", cfg.ReasoningEmission)
	}
}

func TestReasoningEmissionRejectsUnknown(t *testing.T) {
	setLoadEnv(t)
	t.Setenv("COPILOT_REASONING_EMISSION", "verbose")

	if _, err := Load(); err == nil {
		t.Fatal("expected unknown COPILOT_REASONING_EMISSION value to be rejected")
	}
}

func setLoadEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"COPILOT_API_ADDR",
		"COPILOT_API_CACHE_DIR",
		"COPILOT_API_CONFIG_DIR",
		"COPILOT_API_DATA_DIR",
		"COPILOT_API_KEY",
		"COPILOT_API_STATE_DIR",
		"COPILOT_CLI_PATH",
		"COPILOT_DEFAULT_REASONING_EFFORT",
		"GITHUB_TOKEN",
		"COPILOT_MODELS_CACHE_TTL",
		"COPILOT_TOOL_CALL_TTL",
		"COPILOT_REQUEST_TIMEOUT",
		"COPILOT_MAX_REQUEST_BODY_BYTES",
		"COPILOT_MAX_TURN_OUTPUT_BYTES",
		"COPILOT_RETENTION_MAX_AGE",
		"COPILOT_RETENTION_MAX_RESPONSES",
		"COPILOT_RETENTION_MAX_BYTES",
		"COPILOT_LOG_CONTENT",
		"COPILOT_REASONING_EMISSION",
		"COPILOT_STRICT_COMPAT",
		"COPILOT_WEBSOCKET_IDLE_TIMEOUT",
		"COPILOT_WEBSOCKET_MAX_LIFETIME",
		"COPILOT_WEBSOCKET_PING_INTERVAL",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("COPILOT_LOG_LEVEL", "info")
}
