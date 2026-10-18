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
	"strings"
	"testing"
)

// ADR 0015 PR-1: the k0s join node fails closed. These tests cover the three
// pieces rendered for JOIN nodes only: the pre-start guard (an ExecStartPre
// inside k0scontroller), the post-start join confirmation, and the gate on the
// argv-keyed wipe in kairos-k0s-apply-args.sh. The scripts are executed under
// bash against a temp directory standing in for /var/lib/k0s and stubbed k0s,
// systemctl, timeout and sleep.

var joinGuardFlavours = []struct {
	name     string
	kubevirt bool
	tmpl     string
}{
	{"capv", false, "k0s_kairos_cloud_config_capv.yaml.tmpl"},
	{"capk", true, "k0s_kairos_cloud_config_capk.yaml.tmpl"},
}

type joinScripts struct {
	guard, confirm, applyArgs, dropIn, postBootstrap string
}

func renderJoinScripts(t *testing.T, kubevirt bool) joinScripts {
	t.Helper()
	out, err := RenderK0sCloudConfig(haCPData("join", kubevirt))
	if err != nil {
		t.Fatalf("render k0s join: %v", err)
	}
	s := joinScripts{
		guard:         extractWriteFile(t, out, "kairos-k0s-join-guard.sh"),
		confirm:       extractWriteFile(t, out, "kairos-k0s-join-confirm.sh"),
		applyArgs:     extractWriteFile(t, out, "kairos-k0s-apply-args.sh"),
		dropIn:        extractWriteFile(t, out, "zz-capi-join-guard.conf"),
		postBootstrap: extractWriteFile(t, out, "kairos-k0s-post-bootstrap.sh"),
	}
	for name, v := range map[string]string{
		"join-guard.sh": s.guard, "join-confirm.sh": s.confirm, "apply-args.sh": s.applyArgs,
		"zz-capi-join-guard.conf": s.dropIn, "post-bootstrap.sh": s.postBootstrap,
	} {
		if v == "" {
			t.Fatalf("join render has no %s", name)
		}
	}
	return s
}

func TestK0sJoinGuard_RenderShape(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for _, fl := range joinGuardFlavours {
		fl := fl
		t.Run(fl.name, func(t *testing.T) {
			s := renderJoinScripts(t, fl.kubevirt)

			// The drop-in runs the guard as a plain ExecStartPre: the script
			// decides its own exit code, so there must be no `|| true` and no `-`.
			if !strings.Contains(s.dropIn, "ExecStartPre=/usr/local/bin/kairos-k0s-join-guard.sh\n") {
				t.Errorf("drop-in does not run the guard as a plain ExecStartPre:\n%s", s.dropIn)
			}
			if strings.Contains(s.dropIn, "||") || strings.Contains(s.dropIn, "ExecStartPre=-") {
				t.Errorf("drop-in must not swallow the guard's exit code:\n%s", s.dropIn)
			}
			// ADR 0004: no oneshot ordered ahead of the k0s unit.
			if strings.Contains(s.dropIn, "Before=") || strings.Contains(s.dropIn, "After=") {
				t.Errorf("drop-in must not add unit ordering:\n%s", s.dropIn)
			}

			for name, script := range map[string]string{"guard": s.guard, "confirm": s.confirm, "apply-args": s.applyArgs} {
				if strings.Contains(script, "{{") {
					t.Errorf("%s script contains a template action", name)
				}
				f := filepath.Join(t.TempDir(), name+".sh")
				if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
					t.Fatal(err)
				}
				if b, err := exec.Command(bashPath, "-n", f).CombinedOutput(); err != nil {
					t.Errorf("%s script is not valid bash: %v\n%s", name, err, b)
				}
			}

			// The confirmation is the first step after the apply-args wait, and
			// precedes the CAPK bootstrap-success trap.
			call := strings.Index(s.postBootstrap, "/usr/local/bin/kairos-k0s-join-confirm.sh\n")
			if call < 0 {
				t.Fatalf("post-bootstrap does not run the join confirmation:\n%s", s.postBootstrap)
			}
			active := strings.Index(s.postBootstrap, "systemctl is-active kairos-k0s-apply-args")
			if status := strings.Index(s.postBootstrap, "k0s status >/dev/null"); active < 0 || active > status ||
				!strings.Contains(s.postBootstrap, "active|activating|reloading)") {
				t.Errorf("post-bootstrap must wait for a detached apply-args before the k0s status loop")
			}
			if wait := strings.Index(s.postBootstrap, "k0s status >/dev/null"); wait < 0 || wait > call {
				t.Errorf("join confirmation must follow the k0s status wait")
			}
			if trap := strings.Index(s.postBootstrap, "trap mark_bootstrap_success"); trap >= 0 && trap < call {
				t.Errorf("join confirmation must run before the bootstrap-success trap is armed")
			}
		})
	}
}

// TestK0sJoinGuard_NoInterpolation reads the template source: the guard and
// confirmation blocks must contain no `{{ ... }}` at all.
func TestK0sJoinGuard_NoInterpolation(t *testing.T) {
	for _, fl := range joinGuardFlavours {
		b, err := os.ReadFile(filepath.Join("templates", fl.tmpl))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		start := strings.Index(src, "  - path: /usr/local/bin/kairos-k0s-join-guard.sh")
		if start < 0 {
			t.Fatalf("%s: guard blocks not found", fl.tmpl)
		}
		// The blocks end at the confirm script's final `exit 1`; the first
		// template action after the guard must come after that.
		next := strings.Index(src[start:], "{{")
		confirmEnd := strings.Index(src[start:], "      exit 1\n  {{- end }}")
		if confirmEnd < 0 {
			t.Fatalf("%s: confirm block terminator not found", fl.tmpl)
		}
		if next < confirmEnd {
			t.Errorf("%s: a template action sits inside the join guard blocks:\n%s", fl.tmpl, src[start:start+next+20])
		}
	}
}

// TestK0sJoinGuard_OnlyJoinRenders pins the scope: init, single, worker and k3s
// renders carry none of it.
func TestK0sJoinGuard_OnlyJoinRenders(t *testing.T) {
	forbidden := []string{
		"kairos-k0s-join-guard", "kairos-k0s-join-confirm", "zz-capi-join-guard",
		".kairos-join-confirmed", ".kairos-join-refused",
	}
	check := func(t *testing.T, out string) {
		t.Helper()
		for _, f := range forbidden {
			if strings.Contains(out, f) {
				t.Errorf("render contains %q but is not a k0s join render", f)
			}
		}
	}
	for _, kv := range []bool{false, true} {
		kv := kv
		name := "capv"
		if kv {
			name = "capk"
		}
		for _, role := range []string{"init", "single", ""} {
			role := role
			t.Run("k0s/"+name+"/"+role, func(t *testing.T) {
				d := haCPData("init", kv)
				d.ControlPlaneRole = role
				if role == "single" || role == "" {
					d.SingleNode = true
				}
				out, err := RenderK0sCloudConfig(d)
				if err != nil {
					t.Fatal(err)
				}
				check(t, out)
			})
		}
		t.Run("k0s/"+name+"/worker", func(t *testing.T) {
			d := TemplateData{Role: "worker", WorkerToken: "tok", Hostname: "w-0", UserName: "kairos", UserGroups: []string{"admin"}, IsKubeVirt: kv}
			out, err := RenderK0sCloudConfig(d)
			if err != nil {
				t.Fatal(err)
			}
			check(t, out)
		})
		t.Run("k3s/"+name+"/join", func(t *testing.T) {
			out, err := RenderK3sCloudConfig(haCPData("join", kv))
			if err != nil {
				t.Fatal(err)
			}
			check(t, out)
		})
	}
	// Metal3 joins share the CAPV template and are join renders.
	t.Run("k0s/metal3/join", func(t *testing.T) {
		d := haCPData("join", false)
		d.Metal3 = true
		out, err := RenderK0sCloudConfig(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"kairos-k0s-join-guard.sh", "kairos-k0s-join-confirm.sh", "zz-capi-join-guard.conf"} {
			if !strings.Contains(out, f) {
				t.Errorf("metal3 join render lacks %s", f)
			}
		}
	})
}

// joinEnv is one scripted execution environment: a temp dir standing in for
// /var/lib/k0s and a bin dir of stubs.
type joinEnv struct {
	dir, k0sDir, bin, log string
}

type joinRun struct {
	members    string // stdout of the stubbed `k0s etcd member-list` ("" with memberRC != 0: it fails)
	memberRC   int
	rmFails    bool
	bare       bool // the live k0scontroller has no --token-file in its argv
	mvFails    bool // writing a marker (mv) fails
	pidChanges bool // MainPID differs between the reads around member-list
}

type joinResult struct {
	exit   int
	output string
	log    []string
}

func (r joinResult) count(prefix string) int {
	n := 0
	for _, l := range r.log {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func newJoinEnv(t *testing.T) *joinEnv {
	t.Helper()
	// Not t.TempDir(): the subtest name would land in the path.
	dir, err := os.MkdirTemp("", "k0s-join-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	e := &joinEnv{dir: dir, k0sDir: filepath.Join(dir, "k0s"), bin: filepath.Join(dir, "bin"), log: filepath.Join(dir, "calls.log")}
	for _, d := range []string{e.k0sDir, e.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *joinEnv) touch(t *testing.T, rel string) {
	t.Helper()
	p := filepath.Join(e.k0sDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *joinEnv) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(e.k0sDir, rel))
	return err == nil
}

func (e *joinEnv) stubs(t *testing.T, extra map[string]string) {
	t.Helper()
	stubs := map[string]string{
		"sleep": "#!/bin/bash\necho \"sleep $*\" >> \"${STUB_LOG}\"\n",
		"timeout": `#!/bin/bash
echo "timeout $*" >> "${STUB_LOG}"
shift
exec "$@"
`,
		"systemctl": "#!/bin/bash\necho \"systemctl $*\" >> \"${STUB_LOG}\"\n",
		"k0s": `#!/bin/bash
echo "k0s $*" >> "${STUB_LOG}"
if [ "$1 $2" = "etcd member-list" ]; then
  printf '%s' "${STUB_MEMBERS}"
  exit "${STUB_MEMBER_RC}"
fi
`,
	}
	for k, v := range extra {
		stubs[k] = v
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(e.bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *joinEnv) run(t *testing.T, bashPath, script string, run joinRun, replace map[string]string) joinResult {
	t.Helper()
	if !strings.Contains(script, "/var/lib/k0s") {
		t.Fatal("script does not reference /var/lib/k0s")
	}
	script = strings.ReplaceAll(script, "/var/lib/k0s", e.k0sDir)
	for from, to := range replace {
		if !strings.Contains(script, from) {
			t.Fatalf("script does not contain %q", from)
		}
		script = strings.ReplaceAll(script, from, to)
	}
	extra := map[string]string{}
	if run.rmFails {
		extra["rm"] = "#!/bin/bash\necho \"rm $*\" >> \"${STUB_LOG}\"\nexit 1\n"
	}
	if run.mvFails {
		extra["mv"] = "#!/bin/bash\necho \"mv $*\" >> \"${STUB_LOG}\"\nexit 1\n"
	}
	if fromProc := `"/proc/$1/cmdline"`; strings.Contains(script, fromProc) {
		// The confirmation reads the live k0scontroller's argv. Two PIDs exist so
		// a test can make MainPID change between its two reads.
		cmdline := "/usr/bin/k0s\x00controller\x00--token-file=/etc/k0s/controller-token\x00"
		if run.bare {
			cmdline = "/usr/bin/k0s\x00controller\x00"
		}
		for _, pid := range []string{"4242", "4243"} {
			procDir := filepath.Join(e.dir, "proc", pid)
			if err := os.MkdirAll(procDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(procDir, "cmdline"), []byte(cmdline), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		pidStub := `#!/bin/bash
echo "systemctl $*" >> "${STUB_LOG}"
case "$*" in *"-p MainPID"*) echo 4242 ;; esac
`
		if run.pidChanges {
			// MainPID alternates between two PIDs on every read.
			pidStub = `#!/bin/bash
echo "systemctl $*" >> "${STUB_LOG}"
case "$*" in
  *"-p MainPID"*)
    n=$(cat "${STUB_STATE}/pidreads" 2>/dev/null || echo 0)
    echo $((n+1)) > "${STUB_STATE}/pidreads"
    echo $((4242 + n % 2))
    ;;
esac
`
		}
		extra["systemctl"] = pidStub
		script = strings.ReplaceAll(script, fromProc, `"`+filepath.Join(e.dir, "proc")+`/$1/cmdline"`)
	}
	e.stubs(t, extra)
	p := filepath.Join(e.dir, "script.sh")
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bashPath, p)
	cmd.Env = append(os.Environ(),
		"PATH="+e.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_LOG="+e.log,
		"STUB_MEMBERS="+run.members,
		"STUB_MEMBER_RC="+string(rune('0'+run.memberRC)),
		"STUB_STATE="+e.dir,
	)
	out, err := cmd.CombinedOutput()
	res := joinResult{output: string(out)}
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run script: %v\n%s", err, out)
		}
		res.exit = ee.ExitCode()
	}
	if b, rerr := os.ReadFile(e.log); rerr == nil {
		res.log = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
	return res
}

func TestK0sJoinGuard_PreStartGuard(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	cases := []struct {
		name       string
		files      []string
		rmFails    bool
		wantExit   int
		wantMember bool // etcd/member still present afterwards
	}{
		{"db without a WAL is cleared", []string{"etcd/member/snap/db"}, false, 0, false},
		{"db with an empty wal dir is cleared", []string{"etcd/member/snap/db", "etcd/member/wal/.keep"}, false, 0, false},
		{"db with only a wal.tmp is cleared", []string{"etcd/member/snap/db", "etcd/member/wal.tmp/0.tmp"}, false, 0, false},
		{"db with a WAL is kept", []string{"etcd/member/snap/db", "etcd/member/wal/0000000000000000-0000000000000000.wal"}, false, 0, true},
		{"db without a WAL but confirmed is kept", []string{"etcd/member/snap/db", ".kairos-join-confirmed"}, false, 0, true},
		{"refusal marker blocks the start", []string{".kairos-join-refused"}, false, 1, false},
		{"refusal marker blocks the start even with a healthy member", []string{".kairos-join-refused", "etcd/member/snap/db", "etcd/member/wal/0.wal"}, false, 1, true},
		{"confirmed beats refused and leaves the member alone", []string{".kairos-join-refused", ".kairos-join-confirmed", "etcd/member/snap/db"}, false, 0, true},
		{"no db is a no-op", nil, false, 0, false},
		{"member dir without a db is a no-op", []string{"etcd/member/wal/0.wal"}, false, 0, true},
		{"a failed removal refuses the start", []string{"etcd/member/snap/db"}, true, 1, true},
	}
	for _, fl := range joinGuardFlavours {
		fl := fl
		script := renderJoinScripts(t, fl.kubevirt).guard
		for _, tc := range cases {
			tc := tc
			t.Run(fl.name+"/"+tc.name, func(t *testing.T) {
				e := newJoinEnv(t)
				for _, f := range tc.files {
					e.touch(t, f)
				}
				res := e.run(t, bashPath, script, joinRun{rmFails: tc.rmFails}, nil)
				if res.exit != tc.wantExit {
					t.Errorf("exit = %d, want %d\n%s", res.exit, tc.wantExit, res.output)
				}
				if got := e.exists("etcd/member"); got != tc.wantMember {
					t.Errorf("etcd/member present = %v, want %v", got, tc.wantMember)
				}
				// The guard's removal never touches the markers.
				for _, m := range []string{".kairos-join-confirmed", ".kairos-join-refused"} {
					for _, f := range tc.files {
						if f == m && !e.exists(m) {
							t.Errorf("marker %s was removed", m)
						}
					}
				}
				// It only inspects files: no k0s, no systemctl.
				if res.count("k0s ") != 0 || res.count("systemctl ") != 0 {
					t.Errorf("guard ran a command it must not: %v", res.log)
				}
			})
		}
	}
}

func TestK0sJoinGuard_Confirmation(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	two := `{"members":{"cp-0":"https://10.0.0.1:2380","cp-1":"https://10.0.0.2:2380"}}`
	three := "{\n  \"members\": {\n    \"a\": \"https://1:2380\",\n    \"b\": \"https://2:2380\",\n    \"c\": \"https://3:2380\"\n  }\n}\n"
	cases := []struct {
		name      string
		files     []string
		run       joinRun
		wantExit  int
		confirmed bool
		refused   bool
		wantStop  bool
		wantPolls int // member-list calls; -1 = do not check
	}{
		{"2 members confirm", nil, joinRun{members: two}, 0, true, false, false, 1},
		{"3 pretty-printed members confirm", nil, joinRun{members: three}, 0, true, false, false, 1},
		{"1 member stops and refuses", nil, joinRun{members: `{"members":{"cp-1":"https://10.0.0.2:2380"}}`}, 1, false, true, true, 1},
		{"1 member but MainPID changed between the reads is inconclusive", nil, joinRun{members: `{"members":{"cp-1":"https://10.0.0.2:2380"}}`, pidChanges: true}, 1, false, false, false, 120},
		{"2 members confirm even when MainPID changes", nil, joinRun{members: two, pidChanges: true}, 0, true, false, false, 1},
		// A bare k0s (started before its arguments existed) is a one-member cluster
		// that apply-args is about to wipe: it must neither refuse nor stop k0s.
		{"bare argv with 1 member is inconclusive, not a refusal", nil, joinRun{members: `{"members":{"cp-1":"https://10.0.0.2:2380"}}`, bare: true}, 1, false, false, false, 120},
		{"bare argv with 2 members still confirms", nil, joinRun{members: two, bare: true}, 0, true, false, false, 1},
		{"a failed confirmed-marker write fails the run", nil, joinRun{members: two, mvFails: true}, 1, false, false, false, 1},
		{"a failed refusal-marker write still stops k0s and fails", nil, joinRun{members: `{"members":{"cp-1":"https://10.0.0.2:2380"}}`, mvFails: true}, 1, false, false, true, 1},
		// GT-10 shape pin: only the keys of the members object count.
		{"nested member values count once", nil, joinRun{members: `{"members":{"cp-1":{"peer":"https://a:2380","x":"y"}}}`}, 1, false, true, true, 1},
		{"a sibling object never adds members", nil, joinRun{members: `{"members":{"cp-1":"https://a:2380"},"other":{"b":"c","d":"e"}}`}, 1, false, true, true, 1},
		{"member-list failing for the whole window writes nothing", nil, joinRun{members: "", memberRC: 1}, 1, false, false, false, 120},
		{"garbage output for the whole window writes nothing", nil, joinRun{members: "not json"}, 1, false, false, false, 120},
		{"empty members for the whole window writes nothing", nil, joinRun{members: `{"members":{}}`}, 1, false, false, false, 120},
		{"already confirmed skips the check", []string{".kairos-join-confirmed"}, joinRun{members: `{"members":{"a":"b"}}`}, 0, true, false, false, 0},
		{"a refusal marker blocks without polling", []string{".kairos-join-refused"}, joinRun{members: two}, 1, false, true, false, 0},
		{"confirmed beats a stale refusal marker", []string{".kairos-join-confirmed", ".kairos-join-refused"}, joinRun{members: `{"members":{"a":"b"}}`}, 0, true, true, false, 0},
	}
	for _, fl := range joinGuardFlavours {
		fl := fl
		script := renderJoinScripts(t, fl.kubevirt).confirm
		for _, tc := range cases {
			tc := tc
			t.Run(fl.name+"/"+tc.name, func(t *testing.T) {
				e := newJoinEnv(t)
				for _, f := range tc.files {
					e.touch(t, f)
				}
				res := e.run(t, bashPath, script, tc.run, nil)
				if res.exit != tc.wantExit {
					t.Errorf("exit = %d, want %d\n%s", res.exit, tc.wantExit, res.output)
				}
				if got := e.exists(".kairos-join-confirmed"); got != tc.confirmed {
					t.Errorf("confirmed marker = %v, want %v", got, tc.confirmed)
				}
				if got := e.exists(".kairos-join-refused"); got != tc.refused {
					t.Errorf("refusal marker = %v, want %v", got, tc.refused)
				}
				if got := res.count("systemctl --no-block stop k0scontroller.service"); (got == 1) != tc.wantStop || got > 1 {
					t.Errorf("systemctl --no-block stop ran %d times, wantStop=%v\n%v", got, tc.wantStop, res.log)
				}
				if got := res.count("k0s etcd member-list"); tc.wantPolls >= 0 && got != tc.wantPolls {
					t.Errorf("member-list ran %d times, want %d", got, tc.wantPolls)
				}
				// Every member-list call is bounded.
				if res.count("k0s etcd member-list") > 0 && res.count("timeout 10 k0s etcd member-list") != res.count("k0s etcd member-list") {
					t.Errorf("member-list not run under timeout 10: %v", res.log[:3])
				}
				// No temp file is left behind next to the markers.
				entries, _ := os.ReadDir(e.k0sDir)
				for _, en := range entries {
					if strings.Contains(en.Name(), ".tmp.") {
						t.Errorf("leftover temp file %s", en.Name())
					}
				}
			})
		}
	}
}

// TestK0sJoinGuard_ApplyArgsWipeGate runs the argv-keyed wipe against a fake
// /proc and a stubbed systemctl. A live process without --token-file wipes the
// self-initialised state, except once the join is confirmed.
func TestK0sJoinGuard_ApplyArgsWipeGate(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	const wantedExec = `{ path=/usr/bin/k0s ; argv[]=/usr/bin/k0s controller --token-file=/etc/k0s/controller-token ; ignore_errors=no }`
	systemctl := `#!/bin/bash
echo "systemctl $*" >> "${STUB_LOG}"
case "$*" in
  *"-p MainPID"*) echo 4242 ;;
  *"-p ExecStart"*) echo '` + wantedExec + `' ;;
esac
`
	cases := []struct {
		name        string
		cmdline     string // NUL-separated argv of the live process
		files       []string
		wantWiped   bool
		wantStopped bool
	}{
		{"missing token and not confirmed: wiped", "/usr/bin/k0s\x00controller\x00", []string{"etcd/member/snap/db", "pki/ca.crt", "bin/k0s", "manifests/x.yaml"}, true, true},
		{"missing token but confirmed: nothing removed", "/usr/bin/k0s\x00controller\x00", []string{"etcd/member/snap/db", "pki/ca.crt", ".kairos-join-confirmed"}, false, false},
		{"token present: nothing removed", "/usr/bin/k0s\x00controller\x00--token-file=/etc/k0s/controller-token\x00", []string{"etcd/member/snap/db"}, false, false},
	}
	for _, fl := range joinGuardFlavours {
		fl := fl
		script := renderJoinScripts(t, fl.kubevirt).applyArgs
		for _, tc := range cases {
			tc := tc
			t.Run(fl.name+"/"+tc.name, func(t *testing.T) {
				e := newJoinEnv(t)
				for _, f := range tc.files {
					e.touch(t, f)
				}
				procDir := filepath.Join(e.dir, "proc", "4242")
				if err := os.MkdirAll(procDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(procDir, "cmdline"), []byte(tc.cmdline), 0o644); err != nil {
					t.Fatal(err)
				}
				e.stubs(t, map[string]string{"systemctl": systemctl})
				res := e.runWithStubs(t, bashPath, script, map[string]string{
					`"/proc/${pid}/cmdline"`: `"` + filepath.Join(e.dir, "proc") + `/${pid}/cmdline"`,
				})
				if res.exit != 0 {
					t.Fatalf("exit = %d\n%s", res.exit, res.output)
				}
				if got := !e.exists("etcd"); got != tc.wantWiped {
					t.Errorf("etcd wiped = %v, want %v\n%s", got, tc.wantWiped, res.output)
				}
				if got := res.count("systemctl stop") > 0; got != tc.wantStopped {
					t.Errorf("systemctl stop called = %v, want %v", got, tc.wantStopped)
				}
				// bin/ and manifests/ always survive; so does a confirmed marker.
				for _, f := range tc.files {
					if strings.HasPrefix(f, "bin/") || strings.HasPrefix(f, "manifests/") || f == ".kairos-join-confirmed" {
						if !e.exists(f) {
							t.Errorf("%s was removed", f)
						}
					}
				}
				// The restart is unconditional once drift is detected.
				if res.count("systemctl restart k0scontroller.service") != 0 && tc.name == "token present: nothing removed" {
					t.Errorf("restart issued although argv matched the token-bearing case")
				}
			})
		}
	}
}

// runWithStubs is run for scripts that need a caller-provided systemctl stub:
// it only rewrites paths and executes, and does not overwrite existing stubs.
func (e *joinEnv) runWithStubs(t *testing.T, bashPath, script string, replace map[string]string) joinResult {
	t.Helper()
	script = strings.ReplaceAll(script, "/var/lib/k0s", e.k0sDir)
	for from, to := range replace {
		if !strings.Contains(script, from) {
			t.Fatalf("script does not contain %q", from)
		}
		script = strings.ReplaceAll(script, from, to)
	}
	p := filepath.Join(e.dir, "script.sh")
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bashPath, p)
	cmd.Env = append(os.Environ(),
		"PATH="+e.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_LOG="+e.log,
	)
	out, err := cmd.CombinedOutput()
	res := joinResult{output: string(out)}
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run script: %v\n%s", err, out)
		}
		res.exit = ee.ExitCode()
	}
	if b, rerr := os.ReadFile(e.log); rerr == nil {
		res.log = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
	return res
}
