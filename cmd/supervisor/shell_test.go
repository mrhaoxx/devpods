// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeStat returns a stub `os.Stat` for the supervisor's shell probe.
// paths is a map of path -> mode (use 0 to mean "missing").
func fakeStat(paths map[string]os.FileMode) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		m, ok := paths[p]
		if !ok || m == 0 {
			return nil, os.ErrNotExist
		}
		return fakeFileInfo{mode: m}, nil
	}
}

type fakeFileInfo struct{ mode os.FileMode }

func (f fakeFileInfo) Name() string       { return "" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func mustContain(t *testing.T, args []string, want string) {
	t.Helper()
	// Match either an exact-equal arg (e.g. "-o") or any arg that
	// contains `want` as a substring (e.g. the merged SetEnv line).
	for _, a := range args {
		if a == want || strings.Contains(a, want) {
			return
		}
	}
	t.Errorf("args missing %q\ngot: %v", want, args)
}

func mustNotContainSubstr(t *testing.T, args []string, substr string) {
	t.Helper()
	for _, a := range args {
		if strings.Contains(a, substr) {
			t.Errorf("args unexpectedly contain %q via %q\nargs: %v", substr, a, args)
			return
		}
	}
}

// Tests assert on DEVPOD_ACTIVE_SHELL (the diagnostic env var that
// reflects supervisor's shell-resolution decision) and that no
// ForceCommand line is emitted (the chosen shell lands via patched
// /etc/passwd, not via ForceCommand — that path would swallow
// `ssh ... -- cmd` invocations).

func TestPrepareShellArgs_ExplicitShell_SetsActiveShell(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "fish" },
		nil,
		fakeStat(map[string]os.FileMode{}),
	)
	mustContain(t, args, "-o")
	mustNotContainSubstr(t, args, "ForceCommand=")
	mustContain(t, args, "DEVPOD_ACTIVE_SHELL=fish")
	mustContain(t, args, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/opt/devpod/bin")
	mustContain(t, args, "TERMINFO=/opt/devpod/share/terminfo")
}

func TestPrepareShellArgs_InvalidShell_FallsBackToBash(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "tcsh" },
		nil,
		fakeStat(map[string]os.FileMode{}),
	)
	mustNotContainSubstr(t, args, "ForceCommand=")
	mustContain(t, args, "DEVPOD_ACTIVE_SHELL=bash")
}

func TestPrepareShellArgs_UnsetEnv_PasswdMissing_FallsBackToBash(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "" },
		func() ([]byte, error) { return nil, os.ErrNotExist },
		fakeStat(map[string]os.FileMode{}),
	)
	mustNotContainSubstr(t, args, "ForceCommand=")
	mustContain(t, args, "DEVPOD_ACTIVE_SHELL=bash")
}

func TestPrepareShellArgs_UnsetEnv_PasswdRootShellExecutable_ReportsPasswdShell(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "" },
		func() ([]byte, error) {
			return []byte("root:x:0:0:root:/root:/bin/bash\nnobody:x:65534:65534::/:/bin/false\n"), nil
		},
		fakeStat(map[string]os.FileMode{"/bin/bash": 0o755}),
	)
	mustNotContainSubstr(t, args, "ForceCommand=")
	// /etc/passwd shell wins — supervisor reports it for diagnostics.
	mustContain(t, args, "DEVPOD_ACTIVE_SHELL=/bin/bash")
}

func TestPrepareShellArgs_UnsetEnv_PasswdRootShellMissing_FallsBackToBash(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "" },
		func() ([]byte, error) {
			return []byte("root:x:0:0:root:/root:/bin/zsh\n"), nil
		},
		fakeStat(map[string]os.FileMode{}),
	)
	mustNotContainSubstr(t, args, "ForceCommand=")
	mustContain(t, args, "DEVPOD_ACTIVE_SHELL=bash")
}

func TestPrepareShellArgs_UnsetEnv_PasswdRootShellNotExecutable_FallsBackToBash(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "" },
		func() ([]byte, error) {
			return []byte("root:x:0:0:root:/root:/bin/bash\n"), nil
		},
		fakeStat(map[string]os.FileMode{"/bin/bash": 0o644}),
	)
	mustNotContainSubstr(t, args, "ForceCommand=")
	mustContain(t, args, "DEVPOD_ACTIVE_SHELL=bash")
}

func TestPrepareShellArgs_UnsetEnv_PasswdRootShellNologinBlacklist_FallsBackToBash(t *testing.T) {
	for _, shell := range []string{"/sbin/nologin", "/usr/sbin/nologin", "/bin/false", "/usr/bin/false"} {
		args := prepareShellArgs(
			func(string) string { return "" },
			func() ([]byte, error) {
				return []byte("root:x:0:0:root:/root:" + shell + "\n"), nil
			},
			fakeStat(map[string]os.FileMode{shell: 0o755}),
		)
		mustNotContainSubstr(t, args, "ForceCommand=")
		mustContain(t, args, "DEVPOD_ACTIVE_SHELL=bash")
		if t.Failed() {
			t.Logf("blacklist case failed for shell=%q", shell)
			return
		}
	}
}

func TestPrepareShellArgs_UnsetEnv_PasswdNoRootLine_FallsBackToBash(t *testing.T) {
	args := prepareShellArgs(
		func(string) string { return "" },
		func() ([]byte, error) {
			return []byte("nobody:x:65534:65534::/:/bin/false\n"), nil
		},
		fakeStat(map[string]os.FileMode{"/bin/false": 0o755}),
	)
	mustNotContainSubstr(t, args, "ForceCommand=")
	mustContain(t, args, "DEVPOD_ACTIVE_SHELL=bash")
}

func TestRewriteRootShell_PatchesExistingRoot(t *testing.T) {
	in := []byte("root:x:0:0:root:/root:/sbin/nologin\nnobody:x:65534:65534::/:/bin/false\n")
	out, changed := rewriteRootPasswd(in, "/opt/devpod/bin/bash", "")
	if !changed {
		t.Fatalf("expected change, got changed=false")
	}
	if !strings.Contains(string(out), "root:x:0:0:root:/root:/opt/devpod/bin/bash\n") {
		t.Errorf("root line not patched:\n%s", out)
	}
	if !strings.Contains(string(out), "nobody:x:65534:65534::/:/bin/false\n") {
		t.Errorf("non-root line lost:\n%s", out)
	}
}

func TestRewriteRootShell_NoChangeWhenShellMatches(t *testing.T) {
	in := []byte("root:x:0:0:root:/root:/opt/devpod/bin/bash\n")
	_, changed := rewriteRootPasswd(in, "/opt/devpod/bin/bash", "")
	if changed {
		t.Errorf("expected changed=false when shell already matches")
	}
}

func TestRewriteRootShell_PrependsWhenNoRootLine(t *testing.T) {
	in := []byte("nobody:x:65534:65534::/:/bin/false\n")
	out, changed := rewriteRootPasswd(in, "/opt/devpod/bin/bash", "")
	if !changed {
		t.Fatalf("expected change")
	}
	if !strings.HasPrefix(string(out), "root:x:0:0:root:/root:/opt/devpod/bin/bash\n") {
		t.Errorf("expected root line at start:\n%s", out)
	}
	if !strings.Contains(string(out), "nobody:") {
		t.Errorf("existing entries lost:\n%s", out)
	}
}

func TestRewriteRootShell_EmptyInputWritesSynthetic(t *testing.T) {
	out, changed := rewriteRootPasswd(nil, "/opt/devpod/bin/bash", "")
	if !changed {
		t.Fatalf("expected change")
	}
	if string(out) != "root:x:0:0:root:/root:/opt/devpod/bin/bash\n" {
		t.Errorf("synthetic line wrong:\n%q", out)
	}
}

func TestPatchPasswdRootShell_NoOpWhenWantEmpty(t *testing.T) {
	called := false
	writer := func(b []byte) error { called = true; return nil }
	err := patchPasswdRootShell(
		"", // not forcing
		"", // no home override
		func() ([]byte, error) {
			return []byte("root:x:0:0:root:/root:/bin/bash\n"), nil
		},
		writer,
	)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if called {
		t.Errorf("writer called even though want is empty")
	}
}

func TestPatchPasswdRootShell_PatchesWhenForcing(t *testing.T) {
	var got []byte
	writer := func(b []byte) error { got = b; return nil }
	err := patchPasswdRootShell(
		"/opt/devpod/bin/zsh",
		"",
		func() ([]byte, error) {
			return []byte("root:x:0:0:root:/root:/sbin/nologin\n"), nil
		},
		writer,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(got), "root:x:0:0:root:/root:/opt/devpod/bin/zsh\n") {
		t.Errorf("expected patched root line, got:\n%s", got)
	}
}

func TestPatchPasswdRootShell_NoChangeIfAlreadyCorrect(t *testing.T) {
	called := false
	writer := func(b []byte) error { called = true; return nil }
	err := patchPasswdRootShell(
		"/opt/devpod/bin/bash",
		"",
		func() ([]byte, error) {
			return []byte("root:x:0:0:root:/root:/opt/devpod/bin/bash\n"), nil
		},
		writer,
	)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if called {
		t.Errorf("writer called even though /etc/passwd already correct")
	}
}

func TestInstallProfileScripts_WritesBothFiles(t *testing.T) {
	dirs := map[string]bool{}
	files := map[string]string{}
	mkdir := func(p string, _ os.FileMode) error {
		dirs[p] = true
		return nil
	}
	write := func(p string, b []byte, _ os.FileMode) error {
		files[p] = string(b)
		return nil
	}
	if err := installProfileScripts(mkdir, write); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dirs["/etc/profile.d"] || !dirs["/etc/fish/conf.d"] {
		t.Errorf("expected both dirs created, got: %v", dirs)
	}
	if got, ok := files["/etc/profile.d/devpod.sh"]; !ok {
		t.Errorf("/etc/profile.d/devpod.sh not written")
	} else if !strings.Contains(got, "/opt/devpod/bin") {
		t.Errorf("profile.d content missing /opt/devpod/bin")
	}
	if got, ok := files["/etc/fish/conf.d/devpod.fish"]; !ok {
		t.Errorf("/etc/fish/conf.d/devpod.fish not written")
	} else if !strings.Contains(got, "/opt/devpod/bin") {
		t.Errorf("fish conf.d content missing /opt/devpod/bin")
	}
}

func TestPatchPasswdRootShell_WritesSyntheticWhenPasswdMissing(t *testing.T) {
	var got []byte
	writer := func(b []byte) error { got = b; return nil }
	err := patchPasswdRootShell(
		"/opt/devpod/bin/fish",
		"",
		func() ([]byte, error) { return nil, os.ErrNotExist },
		writer,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "root:x:0:0:root:/root:/opt/devpod/bin/fish\n" {
		t.Errorf("synthetic line wrong:\n%q", got)
	}
}
