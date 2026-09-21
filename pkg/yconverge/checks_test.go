package yconverge

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func testLogger(t *testing.T) *zap.Logger {
	t.Helper()
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatal(err)
	}
	return logger
}

func TestCheckRunner_ExecSuccess(t *testing.T) {
	runner := &CheckRunner{
		Context:   "test",
		Namespace: "default",
		Logger:    testLogger(t),
	}
	checks := []Check{{
		Kind:        "exec",
		Command:     "true",
		Timeout:     "5s",
		Description: "always succeeds",
	}}
	if err := runner.RunAll(context.Background(), checks); err != nil {
		t.Fatalf("expected success: %v", err)
	}
}

func TestCheckRunner_ExecFailure(t *testing.T) {
	runner := &CheckRunner{
		Context:   "test",
		Namespace: "default",
		Logger:    testLogger(t),
	}
	checks := []Check{{
		Kind:        "exec",
		Command:     "false",
		Timeout:     "3s",
		Description: "always fails",
	}}
	err := runner.RunAll(context.Background(), checks)
	if err == nil {
		t.Fatal("expected error")
	}
	checkErr, ok := err.(*CheckError)
	if !ok {
		t.Fatalf("expected CheckError, got %T", err)
	}
	if checkErr.Index != 0 {
		t.Fatalf("expected index 0, got %d", checkErr.Index)
	}
}

func TestCheckRunner_ExecRetries(t *testing.T) {
	// Create a file that the command checks -- first calls fail, last succeeds
	dir := t.TempDir()
	runner := &CheckRunner{
		Context:   "test",
		Namespace: "default",
		Logger:    testLogger(t),
	}

	// Command that succeeds on 2nd+ attempt (creates a marker file on first run)
	checks := []Check{{
		Kind:        "exec",
		Command:     "test -f " + dir + "/marker || (touch " + dir + "/marker && false)",
		Timeout:     "10s",
		Description: "fails first, succeeds after retry",
	}}

	start := time.Now()
	if err := runner.RunAll(context.Background(), checks); err != nil {
		t.Fatalf("expected success after retry: %v", err)
	}
	elapsed := time.Since(start)
	// Should have retried at least once (2s interval)
	if elapsed < 2*time.Second {
		t.Fatalf("expected retry delay, elapsed=%v", elapsed)
	}
}

func TestCheckRunner_ExecUsesNamespaceEnv(t *testing.T) {
	runner := &CheckRunner{
		Context:   "test-ctx",
		Namespace: "my-ns",
		Logger:    testLogger(t),
	}
	checks := []Check{{
		Kind:        "exec",
		Command:     `test "$NAMESPACE" = "my-ns" && test "$CONTEXT" = "test-ctx"`,
		Timeout:     "5s",
		Description: "env vars are set",
	}}
	if err := runner.RunAll(context.Background(), checks); err != nil {
		t.Fatalf("expected NAMESPACE/CONTEXT env vars: %v", err)
	}
}

func TestCheckRunner_StopsOnFirstFailure(t *testing.T) {
	runner := &CheckRunner{
		Context:   "test",
		Namespace: "default",
		Logger:    testLogger(t),
	}
	checks := []Check{
		{Kind: "exec", Command: "true", Timeout: "5s", Description: "pass"},
		{Kind: "exec", Command: "false", Timeout: "3s", Description: "fail"},
		{Kind: "exec", Command: "true", Timeout: "5s", Description: "never reached"},
	}
	err := runner.RunAll(context.Background(), checks)
	if err == nil {
		t.Fatal("expected error")
	}
	checkErr := err.(*CheckError)
	if checkErr.Index != 1 {
		t.Fatalf("expected failure at index 1, got %d", checkErr.Index)
	}
}

func TestCheckRunner_EmptyChecks(t *testing.T) {
	runner := &CheckRunner{
		Context:   "test",
		Namespace: "default",
		Logger:    testLogger(t),
	}
	if err := runner.RunAll(context.Background(), nil); err != nil {
		t.Fatalf("expected success for empty checks: %v", err)
	}
}

func TestCheckRunner_UnknownKind(t *testing.T) {
	runner := &CheckRunner{
		Context:   "test",
		Namespace: "default",
		Logger:    testLogger(t),
	}
	checks := []Check{{
		Kind:    "unknown",
		Timeout: "5s",
	}}
	err := runner.RunAll(context.Background(), checks)
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestParseDuration_Default(t *testing.T) {
	d, err := parseDuration("")
	if err != nil {
		t.Fatal(err)
	}
	if d != 60*time.Second {
		t.Fatalf("expected 60s, got %v", d)
	}
}

func TestParseDuration_Explicit(t *testing.T) {
	d, err := parseDuration("120s")
	if err != nil {
		t.Fatal(err)
	}
	if d != 120*time.Second {
		t.Fatalf("expected 120s, got %v", d)
	}
}

func TestCheckError_Format(t *testing.T) {
	err := &CheckError{
		Index: 2,
		Check: Check{
			Kind:        "rollout",
			Resource:    "deployment/app",
			Description: "app ready",
		},
		Err: context.DeadlineExceeded,
	}
	got := err.Error()
	if got != "check 2 (app ready): context deadline exceeded" {
		t.Fatalf("unexpected error string: %q", got)
	}
}

// A command that hangs must not outlive the timeout the check was
// given. Before the per-attempt deadline it ran under the parent
// context only, and the timeout was compared between attempts.
func TestCheckRunner_ExecHangingCommandHonoursTimeout(t *testing.T) {
	runner := &CheckRunner{Context: "test", Namespace: "default", Logger: testLogger(t)}
	start := time.Now()
	err := runner.RunAll(context.Background(), []Check{{
		Kind:    "exec",
		Command: "sleep 60",
		Timeout: "1s",
	}})
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("want a timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hanging command held the check for %s", elapsed)
	}
}

// "exit status 1" alone says nothing; the failing command's own
// output is the diagnosis.
func TestCheckRunner_ExecFailureCarriesOutput(t *testing.T) {
	runner := &CheckRunner{Context: "test", Namespace: "default", Logger: testLogger(t)}
	err := runner.RunAll(context.Background(), []Check{{
		Kind:    "exec",
		Command: "echo 'pod db-0 is not ready' >&2; exit 3",
		Timeout: "1s",
	}})
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"exit status 3", "pod db-0 is not ready"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

// The passing attempt's output is shown once; failed attempts before
// it are not.
func TestCheckRunner_ExecSuccessOutputIsShownOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	var out bytes.Buffer
	runner := &CheckRunner{Context: "test", Namespace: "default", Logger: testLogger(t), Stdout: &out}
	err := runner.RunAll(context.Background(), []Check{{
		Kind:    "exec",
		Command: "if test -f " + marker + "; then echo ready; else touch " + marker + "; echo not-yet; exit 1; fi",
		Timeout: "10s",
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "ready\n") {
		t.Errorf("passing attempt's output missing: %q", got)
	}
	if strings.Contains(got, "not-yet") {
		t.Errorf("a failed attempt's output leaked into progress: %q", got)
	}
}
