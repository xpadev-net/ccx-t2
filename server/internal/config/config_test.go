package config

import (
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
