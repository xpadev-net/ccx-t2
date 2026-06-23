package harness

import (
	"strings"
	"testing"

	"github.com/xpadev/ccx-t2/internal/config"
)

func TestResolveDefaultsSingleWorkerHarness(t *testing.T) {
	cfg := testConfig([]string{"sh"}, map[string]config.HarnessConfig{
		"sh": {Command: "sh", McpArgs: "--mcp-url {url}"},
	})

	name, hCfg, err := Resolve(cfg, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if name != "sh" || hCfg.Command != "sh" {
		t.Fatalf("Resolve = %q/%#v, want sh config", name, hCfg)
	}
}

func TestResolveRequiresHarnessWhenMultipleWorkerHarnesses(t *testing.T) {
	cfg := testConfig([]string{"sh", "alt"}, map[string]config.HarnessConfig{
		"sh":  {Command: "sh", McpArgs: "--mcp-url {url}"},
		"alt": {Command: "sh", McpArgs: "--mcp-url {url}"},
	})

	_, _, err := Resolve(cfg, "")
	if err == nil {
		t.Fatal("Resolve error = nil, want missing harness error")
	}
	for _, want := range []string{"harness argument required", "list_harnesses", "spawn_worker"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Resolve error = %q, want %q", err, want)
		}
	}
}

func TestResolveExplicitAllowedHarness(t *testing.T) {
	cfg := testConfig([]string{"sh", "alt"}, map[string]config.HarnessConfig{
		"sh":  {Command: "sh", McpArgs: "--mcp-url {url}"},
		"alt": {Command: "sh", McpArgs: "--mcp-url {url}"},
	})

	name, hCfg, err := Resolve(cfg, "alt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if name != "alt" || hCfg.Command != "sh" {
		t.Fatalf("Resolve = %q/%#v, want alt config", name, hCfg)
	}
}

func TestResolveExplicitUnavailableHarness(t *testing.T) {
	cfg := testConfig([]string{"missing"}, map[string]config.HarnessConfig{
		"missing": {Command: "/definitely/missing/ccx-t2-harness", McpArgs: "--mcp-url {url}"},
	})

	_, _, err := Resolve(cfg, "missing")
	if err == nil {
		t.Fatal("Resolve error = nil, want unavailable harness error")
	}
	for _, want := range []string{"harness \"missing\" binary", "not found in PATH"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Resolve error = %q, want %q", err, want)
		}
	}
}

func TestResolveExplicitNonAllowlistedHarness(t *testing.T) {
	cfg := testConfig([]string{"sh"}, map[string]config.HarnessConfig{
		"sh":    {Command: "sh", McpArgs: "--mcp-url {url}"},
		"other": {Command: "sh", McpArgs: "--mcp-url {url}"},
	})

	_, _, err := Resolve(cfg, "other")
	if err == nil {
		t.Fatal("Resolve error = nil, want non-allowlisted harness error")
	}
	if !strings.Contains(err.Error(), `harness "other" is not in worker_harnesses`) {
		t.Fatalf("Resolve error = %q, want non-allowlisted error", err)
	}
}

func testConfig(workerHarnesses []string, harnesses map[string]config.HarnessConfig) *config.Config {
	return &config.Config{
		WorkerHarnesses: workerHarnesses,
		Harnesses:       harnesses,
	}
}
