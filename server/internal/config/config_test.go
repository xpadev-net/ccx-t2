package config

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestValidateHarnessRejectsInvalidMCPArgsShellSyntax(t *testing.T) {
	cfg := &Config{
		Harnesses: map[string]HarnessConfig{
			"worker": {
				Command: "codex",
				McpArgs: "--mcp-url '{url}",
			},
		},
	}

	err := validateHarness(cfg, "worker", false)
	if err == nil {
		t.Fatal("validateHarness() error = nil, want invalid mcp_args shell syntax error")
	}
	if !strings.Contains(err.Error(), "mcp_args has invalid shell syntax") {
		t.Fatalf("validateHarness() error = %v, want mcp_args shell syntax error", err)
	}
}

func TestExpandEnvWarnsWhenReferenceExpandsEmpty(t *testing.T) {
	t.Setenv("MISSING_SECRET_FOR_TEST", "")
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	cfg := &Config{Server: ServerConfig{McpSecret: "${MISSING_SECRET_FOR_TEST}"}}
	expandEnv(cfg)

	if cfg.Server.McpSecret != "" {
		t.Fatalf("McpSecret = %q, want empty", cfg.Server.McpSecret)
	}
	if !strings.Contains(buf.String(), "MISSING_SECRET_FOR_TEST") {
		t.Fatalf("log output %q does not mention missing env var", buf.String())
	}
}
