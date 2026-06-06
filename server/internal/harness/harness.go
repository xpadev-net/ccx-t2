// Package harness provides utilities for managing harness binaries and
// determining which harness to use for a given task.
package harness

import (
	"fmt"
	"os/exec"

	"github.com/xpadev/ccx-t2/internal/config"
)

// Info describes a harness with its availability.
type Info struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Usage     any    `json:"usage"`
}

// List returns availability information for all worker harnesses.
func List(cfg *config.Config) []Info {
	out := make([]Info, 0, len(cfg.WorkerHarnesses))
	for _, name := range cfg.WorkerHarnesses {
		h, ok := cfg.Harnesses[name]
		available := false
		if ok && h.Command != "" {
			_, err := exec.LookPath(h.Command)
			available = err == nil
		}
		usage := map[string]any{"note": "unavailable"}
		if available {
			usage = map[string]any{"command": h.Command, "mcp_args": h.McpArgs}
		}
		out = append(out, Info{
			Name:      name,
			Available: available,
			Usage:     usage,
		})
	}
	return out
}

// Resolve returns the harness config for a spawn_worker call.
// If harnessName is empty and there is exactly one worker harness, it is used.
// If there are multiple worker harnesses and harnessName is empty, an error is returned.
func Resolve(cfg *config.Config, harnessName string) (string, config.HarnessConfig, error) {
	if harnessName == "" {
		if len(cfg.WorkerHarnesses) == 1 {
			harnessName = cfg.WorkerHarnesses[0]
		} else {
			return "", config.HarnessConfig{}, fmt.Errorf("harness argument required when multiple worker_harnesses are configured")
		}
	}

	// Ensure the harness is in the worker_harnesses allowlist.
	allowed := false
	for _, name := range cfg.WorkerHarnesses {
		if name == harnessName {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", config.HarnessConfig{}, fmt.Errorf("harness %q is not in worker_harnesses", harnessName)
	}

	h, ok := cfg.Harnesses[harnessName]
	if !ok {
		return "", config.HarnessConfig{}, fmt.Errorf("harness %q not found in harnesses config", harnessName)
	}
	// Binary availability is checked at spawn time.
	if _, err := exec.LookPath(h.Command); err != nil {
		return "", config.HarnessConfig{}, fmt.Errorf("harness %q binary %q not found in PATH: %w", harnessName, h.Command, err)
	}
	return harnessName, h, nil
}
