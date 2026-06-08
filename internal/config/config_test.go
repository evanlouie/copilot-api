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
		"COPILOT_DEFAULT_REASONING_EFFORT",
		"COPILOT_MODELS_CACHE_TTL",
		"COPILOT_TOOL_CALL_TTL",
		"COPILOT_REQUEST_TIMEOUT",
		"COPILOT_MAX_REQUEST_BODY_BYTES",
		"COPILOT_LOG_CONTENT",
		"COPILOT_REASONING_EMISSION",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("COPILOT_LOG_LEVEL", "info")
}
