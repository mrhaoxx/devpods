// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Slurm's commands, for those who know them and their scripts: sbatch
// (its common options and #SBATCH lines, -p naming the cluster), squeue,
// scancel — the queue's job submit, list and cancel underneath. What
// Slurm can do and the queue cannot is refused by name.

func slurmCommands() []*command {
	return []*command{
		{group: "Queue", name: "sbatch", raw: true, args: "[options] script [args…]",
			short: "submit a batch script, as Slurm's sbatch (-N, --gpus-per-node, -G, -c, -t, -J, -o, -e, -p, #SBATCH lines)",
			long:  sbatchLong, run: runSbatch},
		{name: "squeue", raw: true, args: "[--me | -u USER]", short: "the queue, as Slurm's squeue", run: runSqueue},
		{name: "scancel", raw: true, args: "<jobid>…", short: "cancel jobs, as Slurm's scancel", run: runScancel},
	}
}

const sbatchLong = `sbatch [options] script [args…]   (or the script on standard input)
Options, on the command line or as #SBATCH lines at the script's top (the
command line wins): -N/--nodes, --gpus-per-node, -G/--gpus (in all),
-c/--cpus-per-task (cores per machine), -t/--time, -J/--job-name,
-o/--output and -e/--error (default slurm-%j.out in the directory it was
submitted from; %j the job, %x its name), -p/--partition (the cluster),
--export ALL|NONE (the shell's environment, ALL by default),
--requeue / --no-requeue (stopped for another: wait again, or end), and
the queue's own --priority N and --preemptible. -n/--ntasks may not be
more than the machines (one rank each). Anything else (--array,
--dependency, --mem, …) is refused by name.`

// sbatchValue / sbatchFlag: what sbatch takes, by its long names (the
// short ones map onto them).
var sbatchValue = map[string]bool{"nodes": true, "gpus-per-node": true, "gpus": true, "cpus-per-task": true, "time": true,
	"job-name": true, "output": true, "error": true, "partition": true, "priority": true, "ntasks": true, "export": true}
var sbatchShort = map[string]string{"N": "nodes", "G": "gpus", "c": "cpus-per-task", "t": "time", "J": "job-name", "o": "output",
	"e": "error", "p": "partition", "n": "ntasks"}

// parseSbatch: what sbatch asks — script's #SBATCH lines (up to its first
// command), then argv's options over them; the script (its name) and the
// arguments after it.
func parseSbatch(script string, argv []string) (submitOpts, error) {
	o := submitOpts{nodes: 1, vars: map[string]string{}, env: map[string]string{}}
	vals := map[string]string{}
	bools := map[string]bool{}
	var requeue *bool // the last of --requeue / --no-requeue said
	read := func(words []string, takeScript bool) error {
		for i := 0; i < len(words); i++ {
			w := words[i]
			if !strings.HasPrefix(w, "-") || w == "-" {
				if takeScript {
					o.script, o.args = w, words[i+1:]
					return nil
				}
				return fmt.Errorf("sbatch: %q is not an option", w)
			}
			name, value, hasValue := strings.Cut(strings.TrimLeft(w, "-"), "=")
			if !strings.HasPrefix(w, "--") {
				// short: -N 2, -N2
				short := name[:1]
				long, ok := sbatchShort[short]
				if !ok {
					return fmt.Errorf("sbatch: -%s is not supported here", short)
				}
				if len(name) > 1 && !hasValue {
					value, hasValue = name[1:], true
				}
				name = long
			}
			switch {
			case name == "preemptible":
				bools[name] = true
				continue
			case name == "requeue" || name == "no-requeue":
				requeue = ptrBool(name == "requeue")
				continue
			case !sbatchValue[name]:
				return fmt.Errorf("sbatch: --%s is not supported here", name)
			}
			if !hasValue {
				if i+1 >= len(words) {
					return fmt.Errorf("sbatch: --%s needs a value", name)
				}
				i++
				value = words[i]
			}
			vals[name] = value
		}
		return nil
	}
	for _, line := range strings.Split(script, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || (strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "#SBATCH")) {
			continue
		}
		if !strings.HasPrefix(t, "#SBATCH") {
			break // the script has begun: #SBATCH further down is a comment
		}
		words, err := Split(strings.TrimSpace(strings.TrimPrefix(t, "#SBATCH")))
		if err != nil {
			return o, fmt.Errorf("sbatch: %s: %v", t, err)
		}
		if err := read(words, false); err != nil {
			return o, err
		}
	}
	if err := read(argv, true); err != nil {
		return o, err
	}
	num := func(name string) (int, bool, error) {
		v, ok := vals[name]
		if !ok {
			return 0, false, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, true, fmt.Errorf("sbatch: --%s takes a number", name)
		}
		return n, true, nil
	}
	if n, ok, err := num("nodes"); err != nil {
		return o, err
	} else if ok {
		o.nodes = n
	}
	if n, ok, err := num("gpus-per-node"); err != nil {
		return o, err
	} else if ok {
		o.gpus = n
	}
	if n, ok, err := num("gpus"); err != nil {
		return o, err
	} else if ok {
		if o.nodes == 0 || n%o.nodes != 0 {
			return o, fmt.Errorf("sbatch: --gpus %d does not split over %d machines", n, o.nodes)
		}
		o.gpus = n / o.nodes
	}
	if n, ok, err := num("ntasks"); err != nil {
		return o, err
	} else if ok && n > o.nodes {
		return o, fmt.Errorf("sbatch: --ntasks %d is more than the machines (%d): the queue runs one rank on each", n, o.nodes)
	}
	if n, ok, err := num("priority"); err != nil {
		return o, err
	} else if ok {
		o.priority = n
	}
	if v, ok := vals["cpus-per-task"]; ok {
		c, err := strconv.ParseFloat(v, 64)
		if err != nil || !(c > 0) {
			return o, errors.New("sbatch: --cpus-per-task takes a number of cores")
		}
		o.cpus = &c
	}
	if v, ok := vals["time"]; ok {
		d, err := parseTime(v)
		if err != nil {
			return o, fmt.Errorf("sbatch: %v", err)
		}
		o.limit = d
	}
	o.title = vals["job-name"]
	if o.title == "" && o.script != "" {
		o.title = o.script
	}
	o.partition = vals["partition"]
	if o.export = vals["export"]; !validExport(o.export) {
		return o, fmt.Errorf("sbatch: --export takes ALL or NONE here (not %q)", o.export)
	}
	o.preemptible = bools["preemptible"]
	o.requeue = requeue
	out, errFile := vals["output"], vals["error"]
	if out == "" {
		out = "slurm-%j.out"
	}
	o.scriptText = outputWrapper(out, errFile, o.title) + script
	return o, nil
}

// outputWrapper: the line that sends a script's output where -o and -e
// say (Slurm's %j and %x), its Pod's log keeping it too.
func outputWrapper(out, errFile, title string) string {
	expand := func(p string) string {
		p = strings.ReplaceAll(p, "%j", "${DEVPOD_JOB}")
		return strings.ReplaceAll(p, "%x", title)
	}
	// the first rank writes the files, as the one batch host does; every
	// rank's own output is in its log all the same
	if errFile == "" || errFile == out {
		return fmt.Sprintf("if [ \"${RANK:-0}\" = 0 ]; then exec > >(tee -a \"%s\") 2>&1; fi\n", expand(out))
	}
	return fmt.Sprintf("if [ \"${RANK:-0}\" = 0 ]; then exec > >(tee -a \"%s\") 2> >(tee -a \"%s\" >&2); fi\n", expand(out), expand(errFile))
}

func runSbatch(e *Env, a *parsed) int {
	c := find("sbatch")
	argv := a.pos
	// the options first: one sbatch does not have is refused by name
	// (before its value could be taken for the script)
	pre, err := parseSbatch("", argv)
	if err != nil {
		fmt.Fprintf(e.Err, "%v\n", err)
		return ExitUsage
	}
	// the script, as dp sent it (or on standard input)
	name := pre.script
	script := ""
	switch text, ok := e.Files[name]; {
	case name != "" && ok:
		script = text
	case name != "":
		return usage(e, c, fmt.Sprintf("cannot read %s here; run sbatch inside a DevPod, or give the script on standard input (ssh … sbatch < %s)", name, name))
	default:
		text, err := readAll(e)
		if err != nil || strings.TrimSpace(text) == "" {
			return usage(e, c, "give a script: sbatch [options] script.sh, or the script on standard input")
		}
		script = text
	}
	o, err := parseSbatch(script, argv)
	if err != nil {
		fmt.Fprintf(e.Err, "%v\n", err)
		return ExitUsage
	}
	if o.script == "" {
		o.script = "stdin"
	}
	cluster, body, err := submitBody(e, o)
	if err != nil {
		return usage(e, c, err.Error())
	}
	var v map[string]any
	if _, err := call(e, "POST", scoped(e, cluster, "/api/jobs"), body, &v); err != nil {
		return fail(e, err)
	}
	sayWarnings(e, false, warningsOf(v)) // DP_NO_WORKDIR_WARNING hides them
	fmt.Fprintf(e.Out, "Submitted batch job %v\n", v["id"])
	return ExitOK
}

// slurmState: a phase as squeue says it.
func slurmState(phase string) string {
	switch phase {
	case "Starting":
		return "CF"
	case "Running":
		return "R"
	case "Succeeded":
		return "CD"
	case "Failed":
		return "F"
	case "TimedOut":
		return "TO"
	case "Cancelled":
		return "CA"
	}
	return "PD"
}

func runSqueue(e *Env, a *parsed) int {
	c := find("squeue")
	user, mine := "", false
	for i := 0; i < len(a.pos); i++ {
		switch w := a.pos[i]; {
		case w == "--me":
			mine = true
		case w == "-u" || w == "--user":
			if i+1 >= len(a.pos) {
				return usage(e, c, "-u needs a user")
			}
			i++
			user = a.pos[i]
		case strings.HasPrefix(w, "--user="):
			user = strings.TrimPrefix(w, "--user=")
		default:
			return usage(e, c, fmt.Sprintf("squeue: %s is not supported here", w))
		}
	}
	path := "/api/jobs"
	if mine {
		path += "?" + url.Values{"mine": {"1"}}.Encode()
	}
	var list struct {
		Items []jobView `json:"items"`
	}
	if _, err := call(e, "GET", path, nil, &list); err != nil {
		return fail(e, err)
	}
	now := e.Now()
	table(e, "JOBID\tPARTITION\tNAME\tUSER\tST\tTIME\tNODES\tNODELIST(REASON)", func(w io.Writer) {
		for _, j := range list.Items {
			if user != "" && j.Owner != user {
				continue
			}
			ran := "0:00"
			if j.StartedAt != nil && j.Phase == "Running" {
				d := now.Sub(*j.StartedAt).Round(time.Second)
				ran = fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
			}
			where := ""
			if j.Phase == "Running" || j.Phase == "Starting" {
				var nodes []string
				for _, r := range j.Ranks {
					if r.Node != "" && !slices.Contains(nodes, r.Node) {
						nodes = append(nodes, r.Node)
					}
				}
				where = strings.Join(nodes, ",")
			} else {
				where = squeueReason(j.Reason)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", j.ID, dash(j.Cluster), dash(j.Title), j.Owner, slurmState(j.Phase), ran, j.Nodes, where)
		}
	})
	return ExitOK
}

func runScancel(e *Env, a *parsed) int {
	c := find("scancel")
	if len(a.pos) == 0 {
		return usage(e, c, "scancel takes job ids")
	}
	status := ExitOK
	for _, ref := range a.pos {
		_, id := splitRef(ref) // st/abc12, as job list says it
		if _, err := call(e, "DELETE", "/api/jobs/"+url.PathEscape(id), nil, nil); err != nil {
			fmt.Fprintf(e.Err, "scancel: %s: %v\n", ref, err)
			status = ExitAPI
		}
	}
	return status
}

// squeueReason: why a job waits, as squeue says it.
func squeueReason(reason string) string {
	r := map[string]string{"capacity": "Resources", "behind": "Priority", "quota": "QOSMaxResources", "preempting": "Resources",
		"preempted": "Priority", "gang-timeout": "Resources", "reserved": "Reservation"}[reason]
	if r == "" {
		r = "None"
	}
	return "(" + r + ")"
}
