// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mrhaoxx/devpod/internal/telemetry/config"
)

// dp is the console's CLI from inside a DevPod (design 2026-11-26): `dp
// job submit …`, `dp reserve …`, and sbatch, squeue and scancel by those
// names. It needs no key: it says it is this DevPod (its UID and its dp
// credential, both the controller's, under /etc/devpod) and the console
// runs the command for the DevPod's owner. The call goes where the
// DevPod's telemetry goes — its cluster's agent, or the console.

// dpTokenPath: the DevPod's dp credential (render.SecretKeyDPToken).
const dpTokenPath = "/etc/devpod/dp-token"

// dpNames: the names the supervisor answers as dp.
var dpNames = []string{"dp", "sbatch", "squeue", "scancel"}

// sbatchValueFlags: sbatch's flags that take the next word as their value
// (so it is not the script).
var sbatchValueFlags = map[string]bool{"-N": true, "-n": true, "-t": true, "-J": true, "-o": true, "-e": true, "-p": true, "-c": true, "-G": true,
	"--nodes": true, "--gpus": true, "--gpus-per-node": true, "--cpus-per-task": true, "--time": true, "--job-name": true,
	"--output": true, "--error": true, "--partition": true, "--priority": true}

// dpMaxFile bounds a script dp sends.
const dpMaxFile = 1 << 20

type dpRequest struct {
	Argv  []string          `json:"argv"`
	Cwd   string            `json:"cwd"`
	Files map[string]string `json:"files,omitempty"`
}

// runDP is dp (or sbatch, squeue, scancel: name) with args.
func runDP(name string, args []string) int {
	b, err := os.ReadFile(config.ConfigPath)
	tok, terr := os.ReadFile(dpTokenPath)
	if err != nil || terr != nil {
		fmt.Fprintf(os.Stderr, "%s: works inside a DevPod with telemetry on (ask an admin)\n", name)
		return 1
	}
	var c config.Config
	if err := json.Unmarshal(b, &c); err != nil || c.DevPodUID == "" || c.IngestURL == "" {
		fmt.Fprintf(os.Stderr, "%s: %s does not say where the console is\n", name, config.ConfigPath)
		return 1
	}
	cwd, _ := os.Getwd()
	argv, files, err := dpArgs(name, args, cwd, os.ReadFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 2
	}
	return dpCall(dpBase(c.IngestURL), c.DevPodUID, strings.TrimSpace(string(tok)), argv, cwd, files, os.Stdout, os.Stderr)
}

// dpBase: the console's address from the DevPod's ingest URL.
func dpBase(ingestURL string) string {
	return strings.TrimSuffix(ingestURL, "/ingest/v1/batch")
}

// dpArgs: the command line the console runs for name and args, and the
// script it names (sbatch's; job submit's --script), read relative to cwd.
func dpArgs(name string, args []string, cwd string, read func(string) ([]byte, error)) ([]string, map[string]string, error) {
	argv := args
	if name != "dp" {
		argv = append([]string{name}, args...)
	}
	script := ""
	switch {
	case len(argv) > 0 && argv[0] == "sbatch":
		for i := 1; i < len(argv); i++ {
			a := argv[i]
			if sbatchValueFlags[a] {
				i++ // its value
				continue
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			script = a
			break
		}
	case len(argv) > 1 && (argv[0] == "job" || argv[0] == "jobs") && argv[1] == "submit":
		for i := 2; i < len(argv); i++ {
			if argv[i] == "--" {
				break
			}
			if v, ok := strings.CutPrefix(argv[i], "--script="); ok {
				script = v
			} else if argv[i] == "--script" && i+1 < len(argv) {
				script = argv[i+1]
			}
		}
	}
	if script == "" {
		return argv, nil, nil
	}
	path := script
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	b, err := read(path)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read %s: %v", script, err)
	}
	if len(b) > dpMaxFile {
		return nil, nil, fmt.Errorf("%s is larger than %d bytes", script, dpMaxFile)
	}
	return argv, map[string]string{script: string(b)}, nil
}

// dpCall posts argv to the console at base as the DevPod uid (token) and
// writes what it says back to out and errw as it comes; it returns the
// command's exit code (1 when the console refused or went away).
func dpCall(base, uid, token string, argv []string, cwd string, files map[string]string, out, errw io.Writer) int {
	body, _ := json.Marshal(dpRequest{Argv: argv, Cwd: cwd, Files: files})
	req, err := http.NewRequest(http.MethodPost, base+"/dp/v1/cli", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(errw, "dp: %v\n", err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-DevPod-UID", uid)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(errw, "dp: the console cannot be reached: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(errw, "dp: the console refused (%d): %s\n", resp.StatusCode, strings.TrimSpace(string(msg)))
		return 1
	}
	rd := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var f struct {
				O *string `json:"o"`
				E *string `json:"e"`
				X *int    `json:"x"`
			}
			if json.Unmarshal(line, &f) == nil {
				switch {
				case f.O != nil:
					_, _ = io.WriteString(out, *f.O)
				case f.E != nil:
					_, _ = io.WriteString(errw, *f.E)
				case f.X != nil:
					return *f.X
				}
			}
		}
		if err != nil {
			fmt.Fprintln(errw, "dp: the connection to the console ended")
			return 1
		}
	}
}
