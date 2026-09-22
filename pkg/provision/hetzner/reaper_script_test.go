package hetzner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/Yolean/y-cluster/pkg/provision/config"
)

// reaperScript extracts the Job's shell script from the rendered
// manifest, so the tests below run what a cluster would run.
func reaperScript(t *testing.T, onExpiry string) string {
	t.Helper()
	for _, doc := range strings.Split(renderReaperManifest(reaperTestOpts(onExpiry), reaperTestNow), "\n---\n") {
		var job struct {
			Kind string `json:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Args []string `json:"args"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &job); err != nil {
			t.Fatalf("parse manifest document: %v", err)
		}
		if job.Kind == "Job" {
			return job.Spec.Template.Spec.Containers[0].Args[0]
		}
	}
	t.Fatal("no Job in the rendered manifest")
	return ""
}

// fakeHCloud writes a stand-in for the hcloud CLI. It logs every call
// and answers from the given /bin/sh `case` arms, matched against the
// full argument string.
func fakeHCloud(t *testing.T, arms string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "hcloud"), filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$*\" in\n" + arms + "\n*) exit 0 ;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// runReaper runs the script with a deadline `untilDeadline` from now
// and returns its exit code, its output and the hcloud calls made.
func runReaper(t *testing.T, onExpiry, hcloudArms string, untilDeadline time.Duration) (int, string, []string) {
	t.Helper()
	bin, log := fakeHCloud(t, hcloudArms)
	cmd := exec.Command("sh", "-c", reaperScript(t, onExpiry))
	cmd.Env = append(os.Environ(),
		"HCLOUD="+bin,
		fmt.Sprintf("EXPIRES_EPOCH=%d", time.Now().Add(untilDeadline).Unix()),
		"MAX_RUN=1h30m0s", "ON_EXPIRY="+onExpiry,
		"SERVER_ID=12345", "LB_ID=67890", "LB_GROUP=alice",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run script: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	var calls []string
	if data, err := os.ReadFile(log); err == nil {
		calls = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	return code, string(out), calls
}

const listOne = `"server list"*) echo "12345 alice-dev running" ;;`
const listTwo = `"server list"*) printf '12345 alice-dev running\n222 alice-qa running\n' ;;`

func TestReaperScript_TeardownOfTheLastMemberDeletesTheLBFirst(t *testing.T) {
	code, out, calls := runReaper(t, config.OnExpiryTeardown, listOne, -time.Minute)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if len(calls) != 3 || !strings.HasPrefix(calls[1], "load-balancer delete 67890") || !strings.HasPrefix(calls[2], "server delete 12345") {
		t.Fatalf("want list, LB delete, server delete; got %q", calls)
	}
}

func TestReaperScript_TeardownLeavesAnLBOthersStillUse(t *testing.T) {
	code, out, calls := runReaper(t, config.OnExpiryTeardown, listTwo, -time.Minute)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "load-balancer delete") {
			t.Fatalf("deleted a load balancer another server is attached to: %q", calls)
		}
	}
	if !strings.HasPrefix(calls[len(calls)-1], "server delete 12345") {
		t.Fatalf("server not deleted: %q", calls)
	}
}

// The failure this guards against: a rotated token or an API outage
// used to read as "0 servers, nothing to do", every hcloud call ended
// in `|| echo`, the Job completed successfully and its TTL removed it
// five minutes later, while the server kept billing.
func TestReaperScript_APIFailureFailsTheJob(t *testing.T) {
	for name, arms := range map[string]string{
		"listing fails":       `"server list"*) echo "hcloud: unable to authenticate" >&2; exit 1 ;;`,
		"server delete fails": listOne + "\n" + `"server delete"*) echo "hcloud: service unavailable" >&2; exit 1 ;;`,
		"LB delete fails":     listOne + "\n" + `"load-balancer delete"*) echo "hcloud: locked" >&2; exit 1 ;;`,
	} {
		t.Run(name, func(t *testing.T) {
			code, out, _ := runReaper(t, config.OnExpiryTeardown, arms, -time.Minute)
			if code == 0 {
				t.Fatalf("the script reported success:\n%s", out)
			}
			if !strings.Contains(out, "FAILED") {
				t.Errorf("the log should say what failed:\n%s", out)
			}
		})
	}
	code, out, _ := runReaper(t, config.OnExpiryStop, `"server shutdown"*) echo "hcloud: unauthorized" >&2; exit 1 ;;`, -time.Minute)
	if code == 0 {
		t.Fatalf("a failed shutdown reported success:\n%s", out)
	}
}

// A retry after a partial teardown finds things gone. That is done,
// not failed.
func TestReaperScript_AlreadyGoneIsDone(t *testing.T) {
	arms := listOne + "\n" +
		`"load-balancer delete"*) echo "hcloud: Load Balancer not found: 67890" >&2; exit 1 ;;` + "\n" +
		`"server delete"*) echo "hcloud: server not found: 12345" >&2; exit 1 ;;`
	code, out, calls := runReaper(t, config.OnExpiryTeardown, arms, -time.Minute)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if len(calls) != 3 {
		t.Fatalf("the server delete must still be attempted after the LB was found gone: %q", calls)
	}
}

func TestReaperScript_StopShutsDownAndDeletesNothing(t *testing.T) {
	code, out, calls := runReaper(t, config.OnExpiryStop, "", -time.Minute)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "server shutdown 12345") {
		t.Fatalf("want exactly one shutdown, got %q", calls)
	}
}

// The script sleeps to the deadline. Before it, nothing happens; once
// it has passed (a retry, a pod recreated after a node reboot) the
// action runs at once instead of after another full budget.
func TestReaperScript_WaitsForTheDeadlineAndNoLonger(t *testing.T) {
	start := time.Now()
	code, out, calls := runReaper(t, config.OnExpiryStop, "", 3*time.Second)
	if code != 0 || len(calls) != 1 {
		t.Fatalf("exit %d calls %q\n%s", code, calls, out)
	}
	if waited := time.Since(start); waited < 2*time.Second {
		t.Errorf("acted %s into a 3s wait", waited)
	}

	start = time.Now()
	runReaper(t, config.OnExpiryStop, "", -time.Hour)
	if waited := time.Since(start); waited > 2*time.Second {
		t.Errorf("a deadline an hour in the past still waited %s", waited)
	}
}
