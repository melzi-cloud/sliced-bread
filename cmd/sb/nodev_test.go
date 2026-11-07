package main

import (
	"os"
	"strings"
	"testing"
)

// The user CLI has no developer commands; those live in hack/sb-dev.
func TestNoDevCommands(t *testing.T) {
	if strings.Contains(userUsage, "sb dev") {
		t.Fatal("user usage mentions sb dev")
	}
	cfg := t.TempDir() + "/config.json"
	if err := os.WriteFile(cfg, []byte(`{"api":"https://api.example.invalid","token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SB_CONFIG", cfg)
	err := runUser([]string{"dev", "tenant", "list"})
	e, ok := err.(*exitErr)
	if !ok || e.code != exitUsage || !strings.Contains(e.msg, "unknown command dev") {
		t.Fatalf("sb dev must be an unknown command, got %v", err)
	}
}
