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
	code := dpCall(srv.URL, "uid-1", "tok", []string{"job", "list"}, "/home/a", map[string]string{"t.sh": "echo"}, &out, &errw)
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
	if code := dpCall(refuse.URL, "uid-1", "tok", []string{"whoami"}, "/", nil, &out, &errw); code != 1 || !bytes.Contains(errw.Bytes(), []byte("not a DevPod's dp")) {
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
}

func TestDPBase(t *testing.T) {
	if b := dpBase("http://a:8081/ingest/v1/batch"); b != "http://a:8081" {
		t.Fatalf("base %q", b)
	}
}
