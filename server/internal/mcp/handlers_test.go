package mcp

import (
	"reflect"
	"testing"

	shellquote "github.com/kballard/go-shellquote"
)

func TestBuildHarnessCommandPreservesExpandedMCPValuesAsSingleArgs(t *testing.T) {
	tokens, err := buildMCPTokens(
		"--mcp-url {url} --mcp-secret {secret} --header 'Authorization: Bearer {secret}'",
		"http://localhost:8080/mcp/worker",
		"secret with spaces; echo 'nope'",
	)
	if err != nil {
		t.Fatalf("buildMCPTokens() error = %v", err)
	}

	command := buildHarnessCommand("codex", tokens)
	got, err := shellquote.Split(command)
	if err != nil {
		t.Fatalf("generated command is not valid shell syntax: %v", err)
	}

	want := []string{
		"codex",
		"--mcp-url",
		"http://localhost:8080/mcp/worker",
		"--mcp-secret",
		"secret with spaces; echo 'nope'",
		"--header",
		"Authorization: Bearer secret with spaces; echo 'nope'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generated args mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestBuildMCPTokensRejectsInvalidTemplateShellSyntax(t *testing.T) {
	_, err := buildMCPTokens("--mcp-url '{url}", "http://localhost:8080/mcp/worker", "")
	if err == nil {
		t.Fatal("buildMCPTokens() error = nil, want invalid shell syntax error")
	}
}
