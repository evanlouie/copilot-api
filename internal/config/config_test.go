package config

import "testing"

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

func setLoadEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"COPILOT_MODELS_CACHE_TTL",
		"COPILOT_TOOL_CALL_TTL",
		"COPILOT_REQUEST_TIMEOUT",
		"COPILOT_MAX_REQUEST_BODY_BYTES",
		"COPILOT_LOG_CONTENT",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("COPILOT_LOG_LEVEL", "info")
}
