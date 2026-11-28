// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const trainScript = "#!/bin/bash\n#SBATCH -N 2\n#SBATCH --gpus-per-node=4\n#SBATCH -t 2:00:00\n#SBATCH -J train\necho go\n#SBATCH -N 9\n"

// #SBATCH lines first (to the first command), the command line over them;
// what is not supported is refused by name.
func TestParseSbatch(t *testing.T) {
	o, err := parseSbatch(trainScript, []string{"-N", "1", "train.sh", "--lr", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if o.nodes != 1 || o.gpus != 4 || o.limit != 2*time.Hour || o.title != "train" || o.script != "train.sh" || !slices.Equal(o.args, []string{"--lr", "1"}) {
		t.Fatalf("opts %+v", o)
	}
	if o, err := parseSbatch("#!/bin/bash\n", []string{"-N2", "-G", "8", "--cpus-per-task=16", "-p", "st2", "--preemptible", "x.sh"}); err != nil || o.nodes != 2 || o.gpus != 4 || o.cpus == nil || *o.cpus != 16 || o.partition != "st2" || !o.preemptible {
		t.Fatalf("flags: %+v %v", o, err)
	}
	for _, bad := range [][]string{{"--array=1-3", "x.sh"}, {"--mem", "4G", "x.sh"}, {"--dependency=afterok:1", "x.sh"}, {"-N", "1", "-n", "4", "x.sh"}} {
		if _, err := parseSbatch("", bad); err == nil || !strings.Contains(err.Error(), strings.SplitN(strings.TrimLeft(bad[0], "-"), "=", 2)[0]) && !strings.Contains(err.Error(), "ntasks") {
			t.Errorf("%v: %v", bad, err)
		}
	}
}

// sbatch from a DevPod: the script runs with its output kept where Slurm
// keeps it; the answer is Slurm's.
func TestSbatchSubmits(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st", "phase": "Queued"}
	code, out, errs := f.runFrom("sbatch train.sh --lr 1", origin, map[string]string{"train.sh": trainScript})
	if code != 0 || strings.TrimSpace(out) != "Submitted batch job abc12" {
		t.Fatalf("sbatch: %d %q %q", code, out, errs)
	}
	b := lastBody(t, f, "POST", "/api/clusters/st/jobs")
	script, _ := b["script"].(string)
	if !strings.HasPrefix(script, `if [ "${RANK:-0}" = 0 ]; then exec > >(tee -a "slurm-${DEVPOD_JOB}.out") 2>&1; fi`+"\n") || !strings.HasSuffix(script, trainScript) || b["workdir"] != "/home/alice/x" ||
		b["nodes"] != float64(2) || b["title"] != "train" {
		t.Fatalf("body %v", b)
	}
	if args, _ := b["args"].([]any); len(args) != 2 || args[0] != "--lr" {
		t.Fatalf("args %v", b["args"])
	}
}

func TestSqueue(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["GET /api/jobs"] = map[string]any{"items": []any{
		map[string]any{"cluster": "st", "id": "run01", "title": "sft", "owner": "alice", "phase": "Running", "nodes": 2, "gpus": 8, "startedAt": "2026-11-27T03:00:00Z",
			"ranks": []any{map[string]any{"rank": 0, "node": "g1"}, map[string]any{"rank": 1, "node": "g2"}}},
		map[string]any{"cluster": "st", "id": "wait1", "title": "pre", "owner": "carol", "phase": "Queued", "nodes": 4, "gpus": 8, "position": 1, "reason": "capacity"},
	}}
	code, out, errs := f.runFrom("squeue", nil, nil)
	if code != 0 {
		t.Fatalf("squeue: %d %q", code, errs)
	}
	for _, want := range []string{"JOBID", "PARTITION", "NAME", "USER", "ST", "TIME", "NODES", "NODELIST(REASON)", "run01", "R", "1:00:00", "g1,g2", "wait1", "PD", "(Resources)"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	for phase, st := range map[string]string{"Queued": "PD", "Starting": "CF", "Running": "R", "Succeeded": "CD", "Failed": "F", "TimedOut": "TO", "Cancelled": "CA"} {
		if got := slurmState(phase); got != st {
			t.Errorf("%s: %q", phase, got)
		}
	}
}

func TestScancel(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["DELETE /api/jobs/abc12"] = nil
	f.status["DELETE /api/jobs/abc12"] = 204
	if code, _, errs := f.run("scancel abc12"); code != 0 || f.calls[len(f.calls)-1] != "DELETE /api/jobs/abc12" {
		t.Fatalf("scancel: %d %q %v", code, errs, f.calls)
	}
}

// What sbatch does not have is refused by name, before any script is
// looked for; a job ID as job list prints it is cancelled as well.
func TestSlurmEdges(t *testing.T) {
	f := newFakeAPI(t)
	if code, _, errs := f.runFrom("sbatch --mem 4G train.sh", origin, nil); code != ExitUsage || !strings.Contains(errs, "--mem is not supported") {
		t.Fatalf("--mem: %d %q", code, errs)
	}
	f.status["DELETE /api/jobs/abc12"] = 204
	f.routes["DELETE /api/jobs/abc12"] = nil
	if code, _, errs := f.run("scancel st/abc12"); code != 0 || f.calls[len(f.calls)-1] != "DELETE /api/jobs/abc12" {
		t.Fatalf("scancel st/abc12: %d %q %v", code, errs, f.calls)
	}
	// another cluster than the DevPod's: its DevPod is not there
	if code, _, errs := f.runFrom("sbatch -p st2 train.sh", origin, map[string]string{"train.sh": trainScript}); code != ExitUsage || !strings.Contains(errs, "st2") {
		t.Fatalf("sbatch -p st2 from st: %d %q", code, errs)
	}
	if code, _, errs := f.runFrom("job submit --cluster st2 --gpus 1 -- true", origin, nil); code != ExitUsage || !strings.Contains(errs, "--from-config") {
		t.Fatalf("job submit --cluster st2 from st: %d %q", code, errs)
	}
}

// sbatch's --requeue / --no-requeue, in the script or on the command
// line (which wins).
func TestSbatchRequeueFlags(t *testing.T) {
	f := newFakeAPI(t)
	f.routes["POST /api/clusters/st/jobs"] = map[string]any{"id": "abc12", "cluster": "st"}
	script := "#!/bin/bash\n#SBATCH --no-requeue\npython t.py\n"
	if code, _, errs := f.runFrom("sbatch t.sh", origin, map[string]string{"t.sh": script}); code != 0 {
		t.Fatalf("script: %d %q", code, errs)
	}
	if b := lastBody(t, f, "POST", "/api/clusters/st/jobs"); b["requeue"] != false {
		t.Fatalf("script body %v", b)
	}
	if code, _, errs := f.runFrom("sbatch --requeue t.sh", origin, map[string]string{"t.sh": script}); code != 0 {
		t.Fatalf("command line: %d %q", code, errs)
	}
	if b := lastBody(t, f, "POST", "/api/clusters/st/jobs"); b["requeue"] != true {
		t.Fatalf("command line body %v", b)
	}
}

// srun's command is the words after its options — or after "--", which
// ends them (srun -c 1 -- sh -c '…'); its options' output files are
// refused, its title is the command's name.
func TestParseSrun(t *testing.T) {
	for _, argv := range [][]string{
		{"-c", "1", "-t", "5", "--", "sh", "-c", "echo hi"},
		{"-c", "1", "-t", "5", "sh", "-c", "echo hi"},
		{"-c1", "-t5", "--", "sh", "-c", "echo hi"},
	} {
		o, err := parseSlurm("srun", "", argv)
		if err != nil {
			t.Fatalf("%q: %v", argv, err)
		}
		if !slices.Equal(o.command, []string{"sh", "-c", "echo hi"}) || o.cpus == nil || *o.cpus != 1 || o.title != "sh" {
			t.Fatalf("%q: command %q cpus %v title %q", argv, o.command, o.cpus, o.title)
		}
	}
	if o, err := parseSlurm("srun", "", []string{"--pty", "--", "/bin/bash", "-l"}); err != nil || !o.pty || !slices.Equal(o.command, []string{"/bin/bash", "-l"}) || o.title != "bash" {
		t.Fatalf("--pty: %+v %v", o, err)
	}
	if o, err := parseSlurm("srun", "", []string{"-c", "1", "--"}); err != nil || len(o.command) != 0 {
		t.Fatalf("nothing after --: %+v %v", o, err)
	}
	if _, err := parseSlurm("srun", "", []string{"-o", "x.log", "true"}); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("-o: %v", err)
	}
}
