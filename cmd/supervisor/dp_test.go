// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// dp posts what it was asked, as its DevPod, and writes back what the
// console says, as it says it; it exits as the command did.
func TestDPCall(t *testing.T) {
	var got dpRequest
	var auth, uid string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dp/v1/cli" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		auth, uid = r.Header.Get("Authorization"), r.Header.Get("X-DevPod-UID")
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprintln(w, `{"o":"hi\n"}`)
		fmt.Fprintln(w, `{"e":"warn\n"}`)
		fmt.Fprintln(w, `{"x":3}`)
	}))
	defer srv.Close()
	var out, errw bytes.Buffer
	code := dpCall(srv.URL, "uid-1", "tok", []string{"job", "list"}, "/home/a", map[string]string{"t.sh": "echo"}, nil, &out, &errw)
	if code != 3 || out.String() != "hi\n" || errw.String() != "warn\n" {
		t.Fatalf("exit %d out %q err %q", code, out.String(), errw.String())
	}
	if auth != "Bearer tok" || uid != "uid-1" || !reflect.DeepEqual(got.Argv, []string{"job", "list"}) || got.Cwd != "/home/a" || got.Files["t.sh"] != "echo" {
		t.Fatalf("sent %q %q %+v", auth, uid, got)
	}
	// refused: said, exit 1
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not a DevPod's dp", http.StatusUnauthorized)
	}))
	defer refuse.Close()
	errw.Reset()
	if code := dpCall(refuse.URL, "uid-1", "tok", []string{"whoami"}, "/", nil, nil, &out, &errw); code != 1 || !bytes.Contains(errw.Bytes(), []byte("not a DevPod's dp")) {
		t.Fatalf("refused: %d %q", code, errw.String())
	}
}

// sbatch, squeue and scancel are themselves; dp passes what follows it.
// The script a command names goes with it.
func TestDPArgvAndFiles(t *testing.T) {
	read := func(p string) ([]byte, error) {
		if p == "/w/train.sh" {
			return []byte("#!/bin/bash\n#SBATCH -N 2\n"), nil
		}
		return nil, errors.New("no such file")
	}
	argv, files, err := dpArgs("sbatch", []string{"-N", "2", "-t", "1:00:00", "train.sh", "--lr", "1"}, "/w", read)
	if err != nil || !reflect.DeepEqual(argv, []string{"sbatch", "-N", "2", "-t", "1:00:00", "train.sh", "--lr", "1"}) || files["train.sh"] != "#!/bin/bash\n#SBATCH -N 2\n" {
		t.Fatalf("sbatch: %v %v %v", argv, files, err)
	}
	argv, files, err = dpArgs("dp", []string{"job", "submit", "--script=train.sh", "-N", "1"}, "/w", read)
	if err != nil || !reflect.DeepEqual(argv, []string{"job", "submit", "--script=train.sh", "-N", "1"}) || files["train.sh"] == "" {
		t.Fatalf("dp job submit --script: %v %v %v", argv, files, err)
	}
	if argv, files, err := dpArgs("squeue", []string{"--me"}, "/w", read); err != nil || !reflect.DeepEqual(argv, []string{"squeue", "--me"}) || len(files) != 0 {
		t.Fatalf("squeue: %v %v %v", argv, files, err)
	}
	if _, _, err := dpArgs("sbatch", []string{"missing.sh"}, "/w", read); err == nil {
		t.Fatal("a script that cannot be read is sent")
	}
	// all of sbatch's value flags skip their value
	if _, files, err := dpArgs("sbatch", []string{"--ntasks", "2", "train.sh"}, "/w", read); err != nil || files["train.sh"] == "" {
		t.Fatalf("--ntasks: %v %v", files, err)
	}
	// an option sbatch does not have goes to the console as it is (it
	// says which): its value is not taken for the script
	if argv, files, err := dpArgs("sbatch", []string{"--mem", "4G", "train.sh"}, "/w", read); err != nil || len(files) != 0 || len(argv) != 4 {
		t.Fatalf("--mem: %v %v %v", argv, files, err)
	}
}

func TestDPBase(t *testing.T) {
	if b := dpBase("http://a:8081/ingest/v1/batch"); b != "http://a:8081" {
		t.Fatalf("base %q", b)
	}
}

// A job's submit (job submit, sbatch) takes the shell's environment along
// (sbatch's --export=ALL), less what is the session's own; other commands
// take none.
func TestDPEnv(t *testing.T) {
	environ := []string{"HOME=/root", "PATH=/opt/conda/bin:/usr/bin", "OMP_NUM_THREADS=8", "SSH_TTY=/dev/pts/0", "DEVPOD_JOB=old",
		"SUPERVISOR_JOB=true", "KUBERNETES_SERVICE_HOST=10.0.0.1", "PWD=/w", "SHLVL=2", "_=/usr/bin/dp", "bad-name=1", "LANG=C.UTF-8"}
	got := dpEnv([]string{"sbatch", "x.sh"}, environ)
	want := map[string]string{"PATH": "/opt/conda/bin:/usr/bin", "OMP_NUM_THREADS": "8", "LANG": "C.UTF-8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sbatch: %v", got)
	}
	if got := dpEnv([]string{"job", "submit", "--", "true"}, environ); !reflect.DeepEqual(got, want) {
		t.Fatalf("job submit: %v", got)
	}
	if got := dpEnv([]string{"squeue"}, environ); got != nil {
		t.Fatalf("squeue: %v", got)
	}
	big := []string{"HUGE=" + strings.Repeat("x", 70<<10), "SMALL=1"}
	if got := dpEnv([]string{"sbatch"}, big); got["HUGE"] != "" || got["SMALL"] != "1" {
		t.Fatalf("over the size: %v", got)
	}
}
