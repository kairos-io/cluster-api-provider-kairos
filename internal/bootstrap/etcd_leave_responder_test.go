/*
Copyright 2024 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing
permissions and limitations under the License.
*/

package bootstrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// etcdLeaveFlavours are the two k0s templates that carry the etcd-leave
// responder. CAPV acks through the VIP, CAPK through the LB endpoint.
var etcdLeaveFlavours = []struct {
	name     string
	kubevirt bool
	server   string // the --server value the rendered script must pass to kubectl
}{
	{"capv", false, "--server=https://192.168.1.240:6443"},
	{"capk", true, "--server=https://10.96.0.10:6443"},
}

func renderEtcdLeaveScript(t *testing.T, kubevirt bool) string {
	t.Helper()
	out, err := RenderK0sCloudConfig(haCPData("init", kubevirt))
	if err != nil {
		t.Fatalf("render k0s init: %v", err)
	}
	script := extractWriteFile(t, out, "kairos-etcd-leave.sh")
	if script == "" {
		t.Fatal("no kairos-etcd-leave.sh rendered")
	}
	return script
}

// TestEtcdLeaveResponder_IsBounded pins the shape of the responder script on
// both backends: every apiserver call has a request timeout, `k0s etcd leave`
// runs under timeout(1), a /run marker records a completed leave, the ack is
// retried inside the run, and the unit has a start timeout. The script must also
// pass `bash -n`.
func TestEtcdLeaveResponder_IsBounded(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available; skipping rendered-script syntax check")
	}
	for _, fl := range etcdLeaveFlavours {
		fl := fl
		t.Run(fl.name, func(t *testing.T) {
			script := renderEtcdLeaveScript(t, fl.kubevirt)

			// Join backslash continuations so each kubectl call is one line.
			joined := strings.ReplaceAll(script, "\\\n", " ")
			calls := 0
			for _, line := range strings.Split(joined, "\n") {
				trim := strings.TrimSpace(line)
				if strings.HasPrefix(trim, "#") || !strings.Contains(trim, "k0s kubectl") {
					continue
				}
				calls++
				if strings.Contains(trim, " patch ") && strings.Contains(trim, "|| true") {
					t.Errorf("the ack must not be fire-and-forget: %s", trim)
				}
				if !strings.Contains(trim, "--request-timeout=10s") {
					t.Errorf("k0s kubectl call has no --request-timeout: %s", trim)
				}
				if !strings.Contains(trim, `"${srv[@]}"`) {
					t.Errorf("k0s kubectl call does not use the shared srv array: %s", trim)
				}
			}
			if calls != 2 {
				t.Errorf("found %d k0s kubectl calls, want 2 (get + patch)", calls)
			}

			for _, want := range []string{
				"timeout 120 k0s etcd leave",         // the leave is bounded
				"command -v timeout",                 // an image without timeout(1) keeps working
				"leave_marker=/run/kairos-etcd-left", // marker lives on tmpfs
				`[ -e "${leave_marker}" ]`,           // marker read: skip the leave
				`touch "${leave_marker}"`,            // marker write: after the leave ran
				"for attempt in {1..24}; do",         // ack retry loop
				"sleep 5",                            // ... spaced 5s apart
				`[ "${val}" = "leave-requested" ]`,   // sentinel gate unchanged
			} {
				if !strings.Contains(script, want) {
					t.Errorf("responder script missing %q", want)
				}
			}
			// A failed leave must never be reported as done, and its stderr must
			// reach the journal.
			for _, forbidden := range []string{"acking anyway", "already left", "etcd leave 2>/dev/null"} {
				if strings.Contains(script, forbidden) {
					t.Errorf("responder script still contains %q", forbidden)
				}
			}

			f := filepath.Join(t.TempDir(), "kairos-etcd-leave.sh")
			if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
				t.Fatalf("write temp script: %v", err)
			}
			if b, err := exec.Command(bashPath, "-n", f).CombinedOutput(); err != nil {
				t.Fatalf("responder script is not valid bash: %v\n%s", err, b)
			}

			// The service unit bounds a whole run.
			out, err := RenderK0sCloudConfig(haCPData("init", fl.kubevirt))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			unit := extractWriteFile(t, out, "kairos-etcd-leave.service")
			if !strings.Contains(unit, "TimeoutStartSec=300") {
				t.Errorf("kairos-etcd-leave.service has no TimeoutStartSec=300:\n%s", unit)
			}
		})
	}
}

// TestEtcdLeaveResponder_NoNewInterpolation locks the template inputs of the
// responder: only the endpoint address, which was already there and is shquoted,
// is interpolated. The check reads the template source, so a new `{{ ... }}`
// anywhere inside the script, service or timer block fails here.
func TestEtcdLeaveResponder_NoNewInterpolation(t *testing.T) {
	actions := regexp.MustCompile(`\{\{.*?\}\}`)
	cases := []struct {
		file string
		want []string
	}{
		{"k0s_kairos_cloud_config_capv.yaml.tmpl", []string{
			"{{- if .VIP }}", "{{ .VIP.Address | shquote }}", "{{- else }}", "{{- end }}",
		}},
		{"k0s_kairos_cloud_config_capk.yaml.tmpl", []string{
			"{{- if .ControlPlaneLBEndpoint }}", "{{ .ControlPlaneLBEndpoint | shquote }}", "{{- else }}", "{{- end }}",
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("templates", tc.file))
			if err != nil {
				t.Fatalf("read template: %v", err)
			}
			src := string(b)
			start := strings.Index(src, "  - path: /usr/local/bin/kairos-etcd-leave.sh")
			end := strings.Index(src, "  - path: /etc/systemd/system/kairos-etcd-leave.timer")
			if start < 0 || end < start {
				t.Fatalf("responder blocks not found in %s", tc.file)
			}
			timerEnd := strings.Index(src[end:], "{{- end }}")
			if timerEnd < 0 {
				t.Fatalf("timer block terminator not found in %s", tc.file)
			}
			// script + service + timer blocks, up to the `{{- end }}` that closes
			// the IsHAControlPlane gate (excluded from the comparison).
			block := src[start : end+timerEnd]
			got := actions.FindAllString(block, -1)
			if len(got) != len(tc.want) {
				t.Fatalf("responder blocks interpolate %v, want exactly %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("action %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// responderRun is one scripted execution of the rendered responder.
type responderRun struct {
	getValue     string // what the stubbed `kubectl get` prints
	markerBefore bool   // /run marker already present
	patchFails   int    // `kubectl patch` fails this many times first
	leaveRC      int    // exit code of the stubbed `k0s etcd leave`
	timeoutFires bool   // the stubbed timeout(1) reports a timeout (124)
}

type responderResult struct {
	exitCode  int
	output    string   // combined stdout and stderr of the script
	log       []string // one entry per stubbed command invocation
	markerSet bool
}

func (r responderResult) count(prefix string) int {
	n := 0
	for _, l := range r.log {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// runResponder executes the rendered script under bash with stubbed k0s,
// timeout, sleep and hostname. The /run marker path is redirected into a temp
// dir; nothing else in the script is altered.
func runResponder(t *testing.T, bashPath, script string, run responderRun) responderResult {
	t.Helper()
	// Not t.TempDir(): its path embeds the subtest name, whose parentheses and
	// spaces the script would see unquoted once the marker path is substituted.
	dir, err := os.MkdirTemp("", "etcd-leave-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "bin")
	state := filepath.Join(dir, "state")
	for _, d := range []string{bin, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	marker := filepath.Join(dir, "etcd-left")
	if !strings.Contains(script, "/run/kairos-etcd-left") {
		t.Fatal("script does not reference /run/kairos-etcd-left")
	}
	script = strings.ReplaceAll(script, "/run/kairos-etcd-left", marker)
	if run.markerBefore {
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}
	logPath := filepath.Join(dir, "calls.log")

	stubs := map[string]string{
		"hostname": "#!/bin/bash\necho cp-0\n",
		"sleep":    "#!/bin/bash\necho \"sleep $*\" >> \"${STUB_LOG}\"\n",
		"timeout": `#!/bin/bash
echo "timeout $*" >> "${STUB_LOG}"
[ -z "${STUB_TIMEOUT_FIRES:-}" ] || exit 124
shift
exec "$@"
`,
		"k0s": `#!/bin/bash
echo "k0s $*" >> "${STUB_LOG}"
case "$1" in
  kubectl)
    case " $* " in
      *" patch "*)
        n=$(cat "${STUB_STATE}/patches" 2>/dev/null || echo 0)
        n=$((n+1))
        echo "${n}" > "${STUB_STATE}/patches"
        [ "${n}" -gt "${STUB_PATCH_FAILS}" ] || exit 1
        ;;
      *" get "*)
        printf '%s' "${STUB_GET_VALUE}"
        ;;
    esac
    ;;
  etcd)
    echo "stub-k0s: etcd leave diagnostics" >&2
    exit "${STUB_LEAVE_RC}"
    ;;
esac
`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	scriptPath := filepath.Join(dir, "kairos-etcd-leave.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command(bashPath, scriptPath)
	env := append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_LOG="+logPath,
		"STUB_STATE="+state,
		"STUB_GET_VALUE="+run.getValue,
		"STUB_PATCH_FAILS="+strconv.Itoa(run.patchFails),
		"STUB_LEAVE_RC="+strconv.Itoa(run.leaveRC),
	)
	if run.timeoutFires {
		env = append(env, "STUB_TIMEOUT_FIRES=1")
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	res := responderResult{output: string(out)}
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run script: %v\n%s", err, out)
		}
		res.exitCode = ee.ExitCode()
	}
	if b, rerr := os.ReadFile(logPath); rerr == nil {
		res.log = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
	if _, serr := os.Stat(marker); serr == nil {
		res.markerSet = true
	}
	return res
}

// TestEtcdLeaveResponder_Behaviour runs the rendered script against stubbed
// commands and checks what a node does in each situation the timer can hand it.
func TestEtcdLeaveResponder_Behaviour(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	const (
		requested = "leave-requested"
		kubectl   = "k0s kubectl"
	)
	cases := []struct {
		name string
		run  responderRun

		wantExitZero bool
		wantLeaves   int
		wantPatches  int
		wantSleeps   int
		wantMarker   bool
	}{
		{
			name:         "no request pending does nothing",
			run:          responderRun{getValue: ""},
			wantExitZero: true,
		},
		{
			name:         "any other value does nothing",
			run:          responderRun{getValue: "left"},
			wantExitZero: true,
		},
		{
			name:         "request leaves once, then acks once",
			run:          responderRun{getValue: requested},
			wantExitZero: true, wantLeaves: 1, wantPatches: 1, wantMarker: true,
		},
		{
			name:         "a completed leave is never re-run, the ack still goes out",
			run:          responderRun{getValue: requested, markerBefore: true},
			wantExitZero: true, wantLeaves: 0, wantPatches: 1, wantMarker: true,
		},
		{
			name:         "ack is retried within the run until it lands",
			run:          responderRun{getValue: requested, patchFails: 3},
			wantExitZero: true, wantLeaves: 1, wantPatches: 4, wantSleeps: 3, wantMarker: true,
		},
		{
			name:         "ack gives up after 24 attempts and the next run resumes at the ack",
			run:          responderRun{getValue: requested, patchFails: 1000},
			wantExitZero: false, wantLeaves: 1, wantPatches: 24, wantSleeps: 23, wantMarker: true,
		},
		{
			name:         "a leave that timed out is not acked and not marked",
			run:          responderRun{getValue: requested, timeoutFires: true},
			wantExitZero: false, wantLeaves: 0, wantPatches: 0, wantMarker: false,
		},
		// Every way the leave can fail leaves the membership unknown. None of
		// them may produce an ack or a marker: the ack would make the controller
		// drop its hook at once, and the marker would pin that result.
		{
			name:         "etcd refusing the removal is not acked and not marked",
			run:          responderRun{getValue: requested, leaveRC: 1},
			wantExitZero: false, wantLeaves: 1, wantPatches: 0, wantMarker: false,
		},
		{
			name:         "a usage or config error (rc 2) is not acked and not marked",
			run:          responderRun{getValue: requested, leaveRC: 2},
			wantExitZero: false, wantLeaves: 1, wantPatches: 0, wantMarker: false,
		},
		{
			name:         "timeout(1) failing to run the command (rc 125) is not acked and not marked",
			run:          responderRun{getValue: requested, leaveRC: 125},
			wantExitZero: false, wantLeaves: 1, wantPatches: 0, wantMarker: false,
		},
		{
			name:         "a command that cannot be executed (rc 126) is not acked and not marked",
			run:          responderRun{getValue: requested, leaveRC: 126},
			wantExitZero: false, wantLeaves: 1, wantPatches: 0, wantMarker: false,
		},
		{
			name:         "a missing command (rc 127) is not acked and not marked",
			run:          responderRun{getValue: requested, leaveRC: 127},
			wantExitZero: false, wantLeaves: 1, wantPatches: 0, wantMarker: false,
		},
		{
			name:         "a killed leave (rc 137) is not acked and not marked",
			run:          responderRun{getValue: requested, leaveRC: 137},
			wantExitZero: false, wantLeaves: 1, wantPatches: 0, wantMarker: false,
		},
	}
	for _, fl := range etcdLeaveFlavours {
		fl := fl
		script := renderEtcdLeaveScript(t, fl.kubevirt)
		for _, tc := range cases {
			tc := tc
			t.Run(fl.name+"/"+tc.name, func(t *testing.T) {
				res := runResponder(t, bashPath, script, tc.run)
				if (res.exitCode == 0) != tc.wantExitZero {
					t.Errorf("exit code = %d, wantExitZero=%v\noutput:\n%s\nlog:\n%s", res.exitCode, tc.wantExitZero, res.output, strings.Join(res.log, "\n"))
				}
				if got := res.count("k0s etcd leave"); got != tc.wantLeaves {
					t.Errorf("k0s etcd leave ran %d times, want %d", got, tc.wantLeaves)
				}
				if got := res.count(kubectl + " " + fl.server + " --request-timeout=10s -n kube-system patch"); got != tc.wantPatches {
					t.Errorf("ack patch ran %d times, want %d\nlog:\n%s", got, tc.wantPatches, strings.Join(res.log, "\n"))
				}
				if got := res.count("sleep 5"); got != tc.wantSleeps {
					t.Errorf("slept %d times, want %d", got, tc.wantSleeps)
				}
				if res.markerSet != tc.wantMarker {
					t.Errorf("marker present = %v, want %v", res.markerSet, tc.wantMarker)
				}
				// Every apiserver call, read or ack, names the endpoint and is bounded.
				for _, l := range res.log {
					if !strings.HasPrefix(l, kubectl) {
						continue
					}
					if !strings.Contains(l, fl.server) || !strings.Contains(l, "--request-timeout=10s") {
						t.Errorf("unbounded or mis-targeted kubectl call: %s", l)
					}
				}
				// The leave's stderr is not discarded.
				if tc.wantLeaves > 0 && !strings.Contains(res.output, "stub-k0s: etcd leave diagnostics") {
					t.Errorf("k0s etcd leave stderr did not reach the output:\n%s", res.output)
				}
				// The leave, when it runs, is wrapped in timeout 120.
				if tc.wantLeaves > 0 || tc.run.timeoutFires {
					if res.count("timeout 120 k0s etcd leave") != 1 {
						t.Errorf("k0s etcd leave was not run under `timeout 120`; log:\n%s", strings.Join(res.log, "\n"))
					}
				}
			})
		}
	}
}
