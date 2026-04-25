package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestServeCmd_RequiresConfig(t *testing.T) {
	cmd := serveCmd()
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "required flag") {
		t.Fatalf("want required-flag error, got %v", err)
	}
}

func TestServeEnsureCmd_RequiresConfig(t *testing.T) {
	cmd := serveEnsureCmd()
	cmd.SetArgs([]string{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "required flag") {
		t.Fatalf("want required-flag error, got %v", err)
	}
}

func TestServeStopCmd_DefaultsOK(t *testing.T) {
	cmd := serveStopCmd()
	// No --state-dir provided → falls through to DefaultStateDir().
	// We don't want to touch the real HOME, so just assert flag parsing.
	cmd.SetArgs([]string{"--help"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "state-dir") {
		t.Fatalf("help output missing flag: %s", out.String())
	}
}

func TestServeLogsCmd_FlagsParse(t *testing.T) {
	cmd := serveLogsCmd()
	cmd.SetArgs([]string{"--follow", "--state-dir", "/tmp/xy-cluster-nonexistent"})
	// We don't run it; reading the log file of a non-existent dir would
	// create the dir due to ResolveStatePaths. Just check flag parsing
	// via --help instead.
	cmd.SetArgs([]string{"--help"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--follow") {
		t.Fatalf("help missing follow flag: %s", out.String())
	}
}

func TestServeCmd_HasSubcommands(t *testing.T) {
	cmd := serveCmd()
	var found = map[string]bool{}
	for _, c := range cmd.Commands() {
		found[c.Name()] = true
	}
	for _, want := range []string{"ensure", "stop", "logs"} {
		if !found[want] {
			t.Fatalf("missing subcommand: %s", want)
		}
	}
}
