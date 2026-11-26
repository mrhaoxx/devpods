// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The queue (design 2026-11-26): submit a job — so many machines, so many
// cards (and cores) on each, for at most so long, at a priority — and it
// waits for the reservable machines' unreserved capacity and runs as one
// DevPod per rank. Inside a DevPod (dp) a job is made from that DevPod
// and runs where dp was asked, in its cluster.

const jobLong = `job submit [opts] -- command…    submit a job (or --script FILE)
job list [--mine] [--ended]       the queues (ended jobs with --ended)
job show <id>                     one job: its ranks, where they run, how they ended
job cancel <id>…                  cancel jobs (a running one is stopped)
job logs [-f] [--rank N] [--attempt N] <id>
                                  a rank's output (live with -f while it runs;
                                  --attempt: an earlier start's, as kept)
job ssh <id> [--rank N]           the SSH login of a rank while it runs

submit options:
  --from-devpod NAME | --from-config NAME [--var K=V]…
                    what the ranks are made from (inside a DevPod: that DevPod)
  -N n              machines, one rank on each (1)
  --gpus n          cards per rank; --cpus n  cores per rank (the cards' share)
  -t TIME           time limit: 30 (minutes), 2:00:00, 1-12, 1-00:30, or 4h (24h)
  --priority p      higher runs first (at most your cap: job submit --dry-run says)
  --preemptible     may be stopped (and queued again) for higher jobs or a
                    reservation; starts sooner
  --host-network --privileged --cap-add CAP   what of the host the ranks ask
  --env K=V  --workdir DIR  --title T  --dry-run (where it would stand)
Each rank runs the command in a login bash with RANK, WORLD_SIZE, MASTER_ADDR,
MASTER_PORT (and DEVPOD_JOB_PORTS) set; the job ends when every rank has.`

// consoleTZ: how the console says times.
var consoleTZ = time.FixedZone("CST", 8*3600)

func jobCommands() []*command {
	return []*command{{group: "Queue", name: "job", aliases: []string{"jobs"},
		args:  "[submit [opts] -- cmd… | list [--mine] [--ended] | show <id> | cancel <id>… | logs [-f] [--rank N] <id> | ssh <id> [--rank N]] [--cluster C]",
		brief: "[submit|list|show|cancel|logs|ssh …]",
		short: "the queue: submit jobs, see the queue, cancel, read a job's output",
		long:  jobLong,
		vals:  []string{"title", "from-devpod", "from-config", "var", "N", "nodes", "gpus", "cpus", "t", "time", "priority", "cap-add", "env", "workdir", "script", "rank", "attempt"},
		bools: []string{"preemptible", "host-network", "privileged", "dry-run", "mine", "ended", "f", "follow"}, run: runJob}}
}

type jobView struct {
	Cluster          string            `json:"cluster"`
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	Owner            string            `json:"owner"`
	Phase            string            `json:"phase"`
	Reason           string            `json:"reason"`
	Detail           map[string]string `json:"detail"`
	Position         int               `json:"position"`
	Nodes            int               `json:"nodes"`
	GPUs             int               `json:"gpus"`
	CPUs             float64           `json:"cpus"`
	TimeLimitSeconds int64             `json:"timeLimitSeconds"`
	Priority         int               `json:"priority"`
	Preemptible      bool              `json:"preemptible"`
	Mine             bool              `json:"mine"`
	Submitted        time.Time         `json:"submitted"`
	ExpectedStart    *time.Time        `json:"expectedStart"`
	StartedAt        *time.Time        `json:"startedAt"`
	FinishedAt       *time.Time        `json:"finishedAt"`
	Ranks            []struct {
		Rank     int    `json:"rank"`
		Node     string `json:"node"`
		DevPod   string `json:"devpod"`
		Phase    string `json:"phase"`
		ExitCode *int   `json:"exitCode"`
	} `json:"ranks"`
	Command string `json:"command"`
	Workdir string `json:"workdir"`
}

func runJob(e *Env, a *parsed) int {
	c := find("job")
	verb, rest := "list", a.pos
	if len(a.pos) > 0 {
		verb, rest = a.pos[0], a.pos[1:]
	}
	a.pos = rest
	switch verb {
	case "submit":
		return jobSubmit(e, a, c)
	case "list", "ls":
		return jobList(e, a)
	case "show":
		return jobShow(e, a, c)
	case "cancel", "rm":
		return each(e, a, c, "cancelled", func(ref string) (string, error) {
			_, id := splitRef(ref)
			_, err := call(e, "DELETE", "/api/jobs/"+url.PathEscape(id), nil, nil)
			return "", err
		})
	case "logs":
		return jobLogs(e, a, c)
	case "ssh":
		return jobSSH(e, a, c)
	}
	return usage(e, c, fmt.Sprintf("unknown job %q: submit, list, show, cancel, logs or ssh", verb))
}

// submitOpts: a job as the command line asks it.
type submitOpts struct {
	title, fromDevPod, fromConfig, workdir, script string
	vars, env                                      map[string]string
	nodes, gpus, priority                          int
	cpus                                           *float64
	limit                                          time.Duration
	preemptible, hostNetwork, privileged, dryRun   bool
	capAdd                                         []string
	command                                        []string
	scriptText                                     string
	args                                           []string // after the script
	partition                                      string   // sbatch -p: the cluster
}

// submitOptsOf reads job submit's flags.
func submitOptsOf(a *parsed) (submitOpts, error) {
	o := submitOpts{nodes: 1, vars: map[string]string{}, env: map[string]string{}}
	o.title, o.fromDevPod, o.fromConfig, o.workdir, o.script = a.val["title"], a.val["from-devpod"], a.val["from-config"], a.val["workdir"], a.val["script"]
	num := func(names []string, dst *int) error {
		for _, n := range names {
			if s, ok := a.val[n]; ok {
				v, err := strconv.Atoi(s)
				if err != nil {
					return fmt.Errorf("--%s takes a number", n)
				}
				*dst = v
			}
		}
		return nil
	}
	if err := num([]string{"N", "nodes"}, &o.nodes); err != nil {
		return o, err
	}
	if err := num([]string{"gpus"}, &o.gpus); err != nil {
		return o, err
	}
	if err := num([]string{"priority"}, &o.priority); err != nil {
		return o, err
	}
	if s, ok := a.val["cpus"]; ok {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || !(v > 0) {
			return o, errors.New("--cpus takes a number of cores")
		}
		o.cpus = &v
	}
	for _, n := range []string{"t", "time"} {
		if s, ok := a.val[n]; ok {
			d, err := parseTime(s)
			if err != nil {
				return o, err
			}
			o.limit = d
		}
	}
	kv := func(name string, into map[string]string) error {
		for _, s := range a.all[name] {
			k, v, ok := strings.Cut(s, "=")
			if !ok || k == "" {
				return fmt.Errorf("--%s takes K=V", name)
			}
			into[k] = v
		}
		return nil
	}
	if err := kv("var", o.vars); err != nil {
		return o, err
	}
	if err := kv("env", o.env); err != nil {
		return o, err
	}
	o.capAdd = a.all["cap-add"]
	o.preemptible, o.hostNetwork, o.privileged, o.dryRun = a.has["preemptible"], a.has["host-network"], a.has["privileged"], a.has["dry-run"]
	o.command = a.pos
	return o, nil
}

// submitBody: the cluster a job goes to and what is posted (inside a
// DevPod its source, working directory and cluster default to it).
func submitBody(e *Env, o submitOpts) (string, map[string]any, error) {
	cluster := e.Cluster
	if o.partition != "" {
		cluster = o.partition
	}
	from := map[string]any{}
	switch {
	case o.fromDevPod != "" && o.fromConfig != "":
		return "", nil, errors.New("--from-devpod or --from-config, not both")
	case o.fromDevPod != "":
		from["devpod"] = o.fromDevPod
	case o.fromConfig != "":
		from["config"] = o.fromConfig
		if len(o.vars) > 0 {
			from["vars"] = o.vars
		}
	case e.Origin != nil && cluster != "" && e.Origin.Cluster != "" && cluster != e.Origin.Cluster:
		return "", nil, fmt.Errorf("this DevPod is in %s, the job would run in %s: say what it is made from there (--from-config NAME, or --from-devpod NAME of a DevPod there)", e.Origin.Cluster, cluster)
	case e.Origin != nil:
		from["devpod"] = e.Origin.DevPod
	default:
		return "", nil, errors.New("say what the job is made from: --from-devpod NAME or --from-config NAME (inside a DevPod: that DevPod)")
	}
	body := map[string]any{"from": from, "nodes": o.nodes, "gpus": o.gpus, "priority": o.priority}
	switch {
	case o.script != "" && len(o.command) > 0:
		return "", nil, errors.New("a command or --script, not both")
	case o.script != "":
		body["script"] = o.scriptText
		if o.title == "" {
			o.title = o.script
		}
	case len(o.command) > 0:
		body["command"] = shellJoin(o.command)
	default:
		return "", nil, errors.New("say what to run: job submit [opts] -- command… (or --script FILE)")
	}
	if o.cpus != nil {
		body["cpus"] = *o.cpus
	}
	if o.limit > 0 {
		body["timeLimitSeconds"] = int64(o.limit / time.Second)
	}
	if o.preemptible {
		body["preemptible"] = true
	}
	priv := map[string]any{}
	if o.hostNetwork {
		priv["hostNetwork"] = true
	}
	if o.privileged {
		priv["privileged"] = true
	}
	if len(o.capAdd) > 0 {
		priv["capAdd"] = o.capAdd
	}
	if len(priv) > 0 {
		body["privileges"] = priv
	}
	if len(o.env) > 0 {
		body["env"] = o.env
	}
	workdir := o.workdir
	if workdir == "" && e.Origin != nil {
		workdir = e.Origin.Cwd
	}
	if workdir != "" {
		body["workdir"] = workdir
	}
	if o.title != "" {
		body["title"] = o.title
	}
	if len(o.args) > 0 {
		body["args"] = o.args
	}
	if o.dryRun {
		body["dryRun"] = true
	}
	return cluster, body, nil
}

func jobSubmit(e *Env, a *parsed, c *command) int {
	o, err := submitOptsOf(a)
	if err != nil {
		return usage(e, c, err.Error())
	}
	if o.script != "" {
		text, ok := e.Files[o.script]
		if !ok {
			if o.script != "-" {
				return usage(e, c, fmt.Sprintf("cannot read %s here; over SSH give --script - and the script on standard input", o.script))
			}
			if text, err = readAll(e); err != nil {
				return fail(e, err)
			}
		}
		o.scriptText = text
	}
	cluster, body, err := submitBody(e, o)
	if err != nil {
		return usage(e, c, err.Error())
	}
	return postJob(e, a, scoped(e, cluster, "/api/jobs"), body)
}

// postJob submits body to path and says how it went.
func postJob(e *Env, a *parsed, path string, body map[string]any) int {
	var v map[string]any
	raw, err := call(e, "POST", path, body, &v)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	if body["dryRun"] == true {
		switch {
		case v["startsNow"] == true:
			fmt.Fprintln(e.Out, "it would start now")
		default:
			line := fmt.Sprintf("it would wait: #%v", v["position"])
			if t, ok := v["expectedStart"].(string); ok {
				if ts, err := time.Parse(time.RFC3339, t); err == nil {
					line += " · expected " + ts.In(consoleTZ).Format("01-02 15:04")
				}
			}
			fmt.Fprintln(e.Out, line)
		}
		return ExitOK
	}
	fmt.Fprintf(e.Out, "submitted job %s\n", joinRef(fmt.Sprint(v["cluster"]), fmt.Sprint(v["id"])))
	return ExitOK
}

func jobList(e *Env, a *parsed) int {
	q := url.Values{}
	if a.has["mine"] {
		q.Set("mine", "1")
	}
	if a.has["ended"] {
		q.Set("ended", "1")
	}
	path := "/api/jobs"
	if e.Cluster != "" {
		path = scoped(e, e.Cluster, path)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var list struct {
		Items []jobView `json:"items"`
	}
	raw, err := call(e, "GET", path, nil, &list)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(e.Out, "the queue is empty; submit a job with `job submit -- command`")
		return ExitOK
	}
	sort.SliceStable(list.Items, func(i, j int) bool {
		pi, pj := list.Items[i].Position, list.Items[j].Position
		if (pi == 0) != (pj == 0) {
			return pi == 0 // running first, then the queue in order
		}
		return pi < pj
	})
	now := e.Now()
	table(e, "ID\tUSER\tSIZE\tPRIORITY\tLIMIT\tSTATE", func(w io.Writer) {
		for _, j := range list.Items {
			id := joinRef(j.Cluster, j.ID)
			if j.Preemptible {
				id += "⚡"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", id, j.Owner, jobSize(j), j.Priority, fmtDur(time.Duration(j.TimeLimitSeconds)*time.Second), jobState(j, now))
		}
	})
	return ExitOK
}

func jobSize(j jobView) string {
	if j.GPUs > 0 {
		return fmt.Sprintf("%d×%d GPU", j.Nodes, j.GPUs)
	}
	return fmt.Sprintf("%d×%s CPU", j.Nodes, strconv.FormatFloat(j.CPUs, 'f', -1, 64))
}

// fmtDur: 3h, 1h30m, 40m.
func fmtDur(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// jobState: where a job is, in a few words.
func jobState(j jobView, now time.Time) string {
	limit := time.Duration(j.TimeLimitSeconds) * time.Second
	switch j.Phase {
	case "Running":
		if j.StartedAt != nil {
			ran := now.Sub(*j.StartedAt)
			return fmt.Sprintf("running %s (≤%s)", fmtDur(ran), fmtDur(limit-ran))
		}
		return "running"
	case "Starting":
		return "starting"
	case "Succeeded":
		return "done"
	case "Failed":
		if x := j.Detail["exit"]; x != "" {
			return "failed (exit " + x + ")"
		}
		if j.Reason == "rank-stopped" {
			return "failed (a rank was stopped)"
		}
		if j.Reason == "gang-timeout" {
			if why := j.Detail["why"]; why != "" {
				return "failed (its ranks never all started: " + why + ")"
			}
			return "failed (its ranks never all started)"
		}
		return "failed"
	case "TimedOut":
		return "out of time"
	case "Cancelled":
		return "cancelled"
	}
	s := fmt.Sprintf("#%d ", j.Position)
	switch j.Reason {
	case "capacity":
		switch fit := j.Detail["fit"]; {
		case fit == "":
			s += fmt.Sprintf("capacity: needs %s, %s free", j.Detail["need"], j.Detail["free"])
		case j.GPUs > 0:
			s += fmt.Sprintf("capacity: %s, %s %s room (%s free)", jobSize(j), fit, plural(fit, "machine has", "machines have"), j.Detail["free"])
		default:
			s += fmt.Sprintf("capacity: %s, %s %s room (%s cores free)", jobSize(j), fit, plural(fit, "machine has", "machines have"), j.Detail["freeCPU"])
		}
	case "behind":
		s += "behind " + j.Detail["head"]
	case "quota":
		s += "over your quota"
	case "preempting":
		s += "making room"
	case "preempted":
		s += "preempted, waiting again"
	case "gang-timeout":
		s += "its ranks did not all start; waiting again"
	case "reserved":
		s += "a reservation took its place; waiting again"
	default:
		s += "waiting"
	}
	if j.ExpectedStart != nil {
		s += " · ~" + j.ExpectedStart.In(consoleTZ).Format("15:04")
	}
	return s
}

func jobShow(e *Env, a *parsed, c *command) int {
	if len(a.pos) != 1 {
		return usage(e, c, "job show takes one job id")
	}
	_, id := splitRef(a.pos[0])
	var j jobView
	raw, err := call(e, "GET", "/api/jobs/"+url.PathEscape(id), nil, &j)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	fmt.Fprintf(e.Out, "job:       %s %s\nuser:      %s\nsize:      %s\npriority:  %d%s\nlimit:     %s\nstate:     %s\n",
		joinRef(j.Cluster, j.ID), j.Title, j.Owner, jobSize(j), j.Priority, map[bool]string{true: " (preemptible)"}[j.Preemptible],
		fmtDur(time.Duration(j.TimeLimitSeconds)*time.Second), jobState(j, e.Now()))
	if j.Command != "" {
		fmt.Fprintf(e.Out, "command:   %s\n", j.Command)
	}
	if j.Workdir != "" {
		fmt.Fprintf(e.Out, "workdir:   %s\n", j.Workdir)
	}
	if len(j.Ranks) > 0 {
		fmt.Fprintln(e.Out)
		table(e, "RANK\tMACHINE\tDEVPOD\tSTATE\tEXIT", func(w io.Writer) {
			for _, r := range j.Ranks {
				x := "-"
				if r.ExitCode != nil {
					x = strconv.Itoa(*r.ExitCode)
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.Rank, dash(r.Node), dash(r.DevPod), dash(r.Phase), x)
			}
		})
	}
	return ExitOK
}

func jobLogs(e *Env, a *parsed, c *command) int {
	if len(a.pos) != 1 {
		return usage(e, c, "job logs takes one job id")
	}
	_, id := splitRef(a.pos[0])
	rank := "0"
	if r, ok := a.val["rank"]; ok {
		if _, err := strconv.Atoi(r); err != nil {
			return usage(e, c, "--rank takes a number")
		}
		rank = r
	}
	path := "/api/jobs/" + url.PathEscape(id) + "/logs?rank=" + rank
	if n, ok := a.val["attempt"]; ok {
		if _, err := strconv.Atoi(n); err != nil {
			return usage(e, c, "--attempt takes a number")
		}
		if a.has["f"] || a.has["follow"] {
			return usage(e, c, "an earlier start's output is kept, not live: drop -f")
		}
		path += "&attempt=" + n
	}
	if a.has["f"] || a.has["follow"] {
		ctx := e.Session
		if ctx == nil {
			ctx = e.Ctx
		}
		// a rank may say nothing for long: only an hour of quiet ends it
		idle := e.FollowIdle
		if idle <= 0 {
			idle = time.Hour
		}
		err := e.API.Stream(ctx, e.User, path+"&follow=1", idle, func(data []byte) error {
			_, err := fmt.Fprintf(e.Out, "%s\n", data)
			return err
		})
		if err != nil {
			return fail(e, err)
		}
		return ExitOK
	}
	raw, err := call(e, "GET", path, nil, nil)
	if err != nil {
		return fail(e, err)
	}
	_, _ = e.Out.Write(raw)
	return ExitOK
}

func jobSSH(e *Env, a *parsed, c *command) int {
	if len(a.pos) != 1 {
		return usage(e, c, "job ssh takes one job id")
	}
	_, id := splitRef(a.pos[0])
	var j jobView
	if _, err := call(e, "GET", "/api/jobs/"+url.PathEscape(id), nil, &j); err != nil {
		return fail(e, err)
	}
	rank := 0
	if r, ok := a.val["rank"]; ok {
		n, err := strconv.Atoi(r)
		if err != nil {
			return usage(e, c, "--rank takes a number")
		}
		rank = n
	}
	if j.Phase != "Running" && j.Phase != "Starting" {
		fmt.Fprintf(e.Err, "error: job %s is not running (%s)\n", id, strings.ToLower(j.Phase))
		return ExitAPI
	}
	var me meView
	if _, err := call(e, "GET", "/api/me", nil, &me); err != nil {
		return fail(e, err)
	}
	ssh := me.sshOf(j.Cluster)
	login := j.Owner + "+" + j.ID + "-" + strconv.Itoa(rank)
	if ssh.LoginSuffix != "" {
		login += "+" + ssh.LoginSuffix
	}
	host, port := ssh.Host, ""
	if host == "" {
		host = "<gateway>"
	} else if ssh.Port != 0 && ssh.Port != 22 {
		port = fmt.Sprintf("-p %d ", ssh.Port)
	}
	fmt.Fprintf(e.Out, "ssh %s%s@%s\n", port, login, host)
	return ExitOK
}

// parseTime reads a time limit as Slurm writes one — minutes, M:S, H:M:S,
// D-H, D-H:M, D-H:M:S — or as a Go duration (4h, 90m).
func parseTime(s string) (time.Duration, error) {
	bad := fmt.Errorf("a time limit is minutes, H:M:S, D-H, D-H:M:S or like 4h; not %q", s)
	if s == "" {
		return 0, bad
	}
	if strings.ContainsAny(s, "hms") {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return 0, bad
		}
		return d, nil
	}
	nums := func(parts []string) ([]int, bool) {
		out := make([]int, len(parts))
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 {
				return nil, false
			}
			out[i] = n
		}
		return out, true
	}
	var d time.Duration
	days, rest, hasDays := strings.Cut(s, "-")
	if hasDays {
		n, ok := nums([]string{days})
		if !ok || rest == "" {
			return 0, bad
		}
		d = time.Duration(n[0]) * 24 * time.Hour
		p, ok := nums(strings.Split(rest, ":"))
		if !ok || len(p) > 3 {
			return 0, bad
		}
		units := []time.Duration{time.Hour, time.Minute, time.Second}
		for i, v := range p {
			d += time.Duration(v) * units[i]
		}
	} else {
		p, ok := nums(strings.Split(s, ":"))
		if !ok || len(p) > 3 {
			return 0, bad
		}
		switch len(p) {
		case 1:
			d = time.Duration(p[0]) * time.Minute
		case 2:
			d = time.Duration(p[0])*time.Minute + time.Duration(p[1])*time.Second
		case 3:
			d = time.Duration(p[0])*time.Hour + time.Duration(p[1])*time.Minute + time.Duration(p[2])*time.Second
		}
	}
	if d <= 0 {
		return 0, bad
	}
	return d, nil
}

// shellJoin quotes words for a shell, as few as need it.
func shellJoin(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		if w != "" && strings.IndexFunc(w, func(r rune) bool {
			return !(r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || r == ':' || r == ',' || r == '+' || r == '@' || r == '%' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
		}) < 0 {
			out[i] = w
			continue
		}
		out[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

// plural: one when n is "1", else many.
func plural(n, one, many string) string {
	if n == "1" {
		return one
	}
	return many
}
