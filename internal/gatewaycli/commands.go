// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/gpures"
)

// Exit statuses.
const (
	ExitOK    = 0
	ExitAPI   = 1 // the API refused or failed
	ExitUsage = 2 // the command line is wrong
)

// Env is one command's surroundings.
type Env struct {
	Ctx  context.Context
	User string // the authenticated caller
	API  *API
	Out  io.Writer
	Err  io.Writer
	Now  func() time.Time

	// Session outlives the command's own time limit (Ctx): long follows
	// run on it. Nil = Ctx.
	Session context.Context
	// Interrupt fires when the user interrupts the command: Ctrl-C on a
	// terminal, an SSH signal, or the channel / connection closing.
	Interrupt <-chan struct{}
	// TTY: the client has a terminal (status lines may be redrawn).
	TTY bool
	// ReadLine prompts on the terminal and reads a line, echoing it or
	// not; nil without a terminal.
	ReadLine func(prompt string, echo bool) (string, error)
	// StdinLine reads the next line of standard input; nil when there is none.
	StdinLine func() (string, error)
	// FollowIdle / FollowBudget override the follow limits (tests).
	FollowIdle, FollowBudget time.Duration
	// Cluster is --cluster: the cluster a command is about, when its names
	// do not say (<cluster>/<name>).
	Cluster string
	// Interactive: the command came from the devpod> prompt.
	Interactive bool
}

type parsed struct {
	cluster string // --cluster, taken by every command
	pos     []string
	val     map[string]string
	all     map[string][]string // every value of a flag given more than once
	has     map[string]bool
	json    bool
}

// command is one row of the table the dispatcher, the parser and the
// help text are all generated from.
type command struct {
	name    string
	aliases []string
	group   string   // the help's heading for it and the commands after
	args    string   // the argument synopsis
	brief   string   // a shorter one for the command list (args when empty)
	short   string   // one line for the list
	long    string   // more for `help <command>`
	admin   bool     // the API only answers admins
	vals    []string // flags that take a value
	bools   []string // flags that do not
	run     func(e *Env, a *parsed) int
}

var commands []*command

func init() {
	edit := []string{"image", "cpu", "memory", "gpu", "gpu-type", "shell"}
	commands = []*command{
		{name: "help", args: "[command]", short: "this list, or one command in full", run: nil},
		{group: "DevPods", name: "list", aliases: []string{"ls"}, args: "[--all] [--cluster C]", short: "list your DevPods (--all: everyone's, admin)", bools: []string{"all"}, run: runList},
		{name: "show", aliases: []string{"status", "get"}, args: "<name>", short: "one DevPod in detail, with its Pod's state", run: runShow},
		{name: "events", args: "<name>", short: "a DevPod's recent events (scheduling, pulls, restarts)", run: runEvents},
		{name: "create", args: "<name> --image IMG [--cpu N] [--memory SIZE] [--gpu N [--gpu-type T] | --gpu kind=N,…] [--shell S] [--collab a,b] [--cluster C]", brief: "<name> --image IMG [flags]",
			short: "create a DevPod",
			long: "<name> is the part after your user name: `create dev1` makes <you>-dev1.\nNon-admins must give --cpu and --memory. To start from a saved config: config launch.\n" +
				"With several clusters, say where: --cluster C, or <cluster>/<name> (create st2/dev1).\n" +
				"--gpu N asks for GPUs of the default kind; --gpu-type T for another (its name or resource, e.g. \"Moore Threads\" or mthreads.com/gpu);\n" +
				"--gpu \"nvidia=1,moore threads=4\" for several kinds at once. In set, a kind not named keeps what it has, and kind=0 gives it up.",
			vals: append([]string{"collab"}, edit...), run: runCreate},
		{name: "start", args: "<name>…", short: "start (wake) DevPods", run: runRunning(true)},
		{name: "stop", args: "<name>…", short: "stop (hibernate) DevPods; the home volume stays", run: runRunning(false)},
		{name: "redeploy", args: "<name>…", short: "recreate the Pod (sessions drop, the home volume stays)", run: runRedeploy},
		{name: "rebuild", args: "<name>… [--dry-run]", short: "rebuild DevPods from the config they were made from, as it is now",
			long:  "A running DevPod's Pod is recreated (sessions drop; the home volume stays).\n--dry-run prints the YAML each would get and changes nothing.",
			bools: []string{"dry-run"}, run: runRebuild},
		{name: "delete", aliases: []string{"rm"}, args: "<name>… --yes", short: "delete DevPods and their data", bools: []string{"yes"}, run: runDelete},
		{name: "set", args: "<name> [--image IMG] [--cpu N] [--memory SIZE] [--gpu N [--gpu-type T] | --gpu kind=N,…] [--shell S]", brief: "<name> [--image|--cpu|--memory|--gpu|--shell …]", short: "change a DevPod (a running one is rebuilt)", vals: edit, run: runSet},
		{name: "yaml", args: "<name> [--apply]", short: "print a DevPod's YAML, or replace it with the YAML on standard input",
			long:  "yaml <name> prints the DevPod as you may edit it. yaml <name> --apply reads a\nwhole new YAML from standard input and replaces the DevPod's spec with it\n(a running DevPod is rebuilt when its Pod changes):\n  ssh <you>+devpod@<gateway> yaml dev1 --apply < dev1.yaml",
			bools: []string{"apply"}, run: runYAML},
		{name: "collab", args: "<name> [add|remove|set user…]", short: "show or change collaborators", run: runCollab},
		{name: "ports", args: "<name> [url <port> | public <port>… | users <port>… | private <port>… | reset]", brief: "<name> [url|public|users|private|reset …]", short: "HTTP addresses of a DevPod's ports",
			long: "Without a subcommand: the address pattern and the public ports.\nurl prints one port's address; public / private open or close a port to\npeople who are not signed in; reset draws a new random address (old links die).",
			run:  runPorts},
		{name: "ssh", args: "<name>", short: "print the ssh command for a DevPod", run: runSSH},
		{name: "migrate", args: "<name> <node|->", short: "pin a DevPod to a node (- lets the scheduler choose)", admin: true, run: runMigrate},
		{group: "Clusters and nodes", name: "clusters", short: "the clusters, where to reach each, and your DevPods in it", run: runClusters},
		{name: "nodes", args: "[--cluster C]", short: "list nodes", admin: true, run: runNodes},
	}
	commands = append(commands, reserveCommands()...)
	commands = append(commands, snapshotCommands()...)
	commands = append(commands, configCommands()...)
	commands = append(commands, &command{group: "You", name: "whoami", short: "who you are, your quota and usage (per cluster)", run: runWhoami})
	commands = append(commands, accountCommands()...)
	commands = append(commands, registryCommands()...)
	commands = append(commands, &command{group: "Admin", name: "users", short: "list users", admin: true, run: runUsers})
}

func find(name string) *command {
	for _, c := range commands {
		if c.name == name || slices.Contains(c.aliases, name) {
			return c
		}
	}
	return nil
}

func (c *command) synopsis() string {
	s := c.name
	if c.args != "" {
		s += " " + c.args
	}
	return s
}

// Help writes the command list, or one command's help.
func Help(w io.Writer, name string) bool { return help(w, name, false) }

// help: in the devpod> shell (interactive) commands are typed as they are.
func help(w io.Writer, name string, interactive bool) bool {
	if name != "" {
		c := find(name)
		if c == nil {
			return false
		}
		fmt.Fprintf(w, "usage: %s\n\n%s", c.synopsis(), c.short)
		if c.admin {
			fmt.Fprint(w, " (admin)")
		}
		fmt.Fprintln(w)
		if c.long != "" {
			fmt.Fprintf(w, "\n%s\n", c.long)
		}
		if strings.Contains(c.args, "<name>") || strings.Contains(c.args, "<id>") || strings.Contains(c.args, "<devpod>") {
			fmt.Fprintln(w, "\nWith several clusters: <cluster>/<name> (or --cluster C) where a name is in more than one.")
		}
		if len(c.aliases) > 0 {
			fmt.Fprintf(w, "\naliases: %s\n", strings.Join(c.aliases, ", "))
		}
		return true
	}
	if interactive {
		fmt.Fprintln(w, "DevPod CLI — type a command; `help <command>` for more, `exit` to leave.")
	} else {
		fmt.Fprintln(w, "DevPod CLI — usage: ssh <user>+devpod@<gateway> <command> [arguments]")
		fmt.Fprintln(w, "(ssh -t <user>+devpod@<gateway> for a devpod> prompt)")
	}
	// one column for every group; a long synopsis is cut to its brief here
	// (help <command> has it in full)
	listed := func(c *command) string {
		if c.brief != "" {
			return c.name + " " + c.brief
		}
		return c.synopsis()
	}
	width := 0
	for _, c := range commands {
		width = max(width, utf8.RuneCountInString(listed(c)))
	}
	for _, c := range commands {
		if c.group != "" {
			fmt.Fprintf(w, "\n%s\n", c.group)
		}
		short := c.short
		if c.admin {
			short += " (admin)"
		}
		s := listed(c)
		fmt.Fprintf(w, "  %s%s  %s\n", s, strings.Repeat(" ", width-utf8.RuneCountInString(s)), short)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, clustersHelp)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Every command takes --json (or -o json): the API's JSON on stdout, for scripts and agents.")
	fmt.Fprintln(w, "Exit status: 0 ok, 1 the API refused, 2 bad command line. Errors go to stderr.")
	return true
}

func parse(c *command, argv []string) (*parsed, error) {
	a := &parsed{val: map[string]string{}, all: map[string][]string{}, has: map[string]bool{}}
	for i := 0; i < len(argv); i++ {
		w := argv[i]
		if w == "--" {
			a.pos = append(a.pos, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			a.pos = append(a.pos, w)
			continue
		}
		name, inline, hasInline := strings.Cut(strings.TrimLeft(w, "-"), "=")
		switch w {
		case "-y":
			name = "yes"
		case "-a":
			name = "all"
		case "-o":
			name = "output"
		}
		value := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			if i+1 >= len(argv) {
				return "", fmt.Errorf("%s needs a value", w)
			}
			i++
			return argv[i], nil
		}
		switch {
		case name == "json":
			a.json = true
		case name == "cluster":
			v, err := value()
			if err != nil {
				return nil, err
			}
			a.cluster = v
		case name == "output":
			v, err := value()
			if err != nil {
				return nil, err
			}
			switch v {
			case "json":
				a.json = true
			case "text", "table":
			default:
				return nil, fmt.Errorf("unknown output format %q (json or text)", v)
			}
		case slices.Contains(c.vals, name):
			v, err := value()
			if err != nil {
				return nil, err
			}
			a.val[name] = v
			a.all[name] = append(a.all[name], v)
		case slices.Contains(c.bools, name):
			a.has[name] = true
		default:
			return nil, fmt.Errorf("unknown flag %s", w)
		}
	}
	return a, nil
}

// Run executes one command line (already split) and returns its exit status.
func Run(e *Env, argv []string) int {
	if e.Now == nil {
		e.Now = time.Now
	}
	if len(argv) == 0 {
		help(e.Out, "", e.Interactive)
		return ExitOK
	}
	name, rest := argv[0], argv[1:]
	if name == "-h" || name == "--help" {
		name = "help"
	}
	if name == "help" {
		topic := ""
		if len(rest) > 0 {
			topic = rest[0]
		}
		if !help(e.Out, topic, e.Interactive) {
			fmt.Fprintf(e.Err, "error: no such command %q; try `help`\n", topic)
			return ExitUsage
		}
		return ExitOK
	}
	c := find(name)
	if c == nil {
		fmt.Fprintf(e.Err, "error: no such command %q; try `help`\n", name)
		return ExitUsage
	}
	if slices.Contains(rest, "--help") || slices.Contains(rest, "-h") {
		help(e.Out, c.name, e.Interactive)
		return ExitOK
	}
	a, err := parse(c, rest)
	if err != nil {
		return usage(e, c, err.Error())
	}
	e.Cluster = a.cluster
	return c.run(e, a)
}

func usage(e *Env, c *command, why string) int {
	fmt.Fprintf(e.Err, "error: %s\nusage: %s\n", why, c.synopsis())
	return ExitUsage
}

func fail(e *Env, err error) int {
	fmt.Fprintf(e.Err, "error: %s\n", err)
	return ExitAPI
}

// call sends a request and decodes the answer into out (when not nil).
func call(e *Env, method, path string, body, out any) ([]byte, error) {
	raw, err := e.API.Do(e.Ctx, e.User, method, path, body)
	if err != nil {
		return nil, explain(err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("unexpected API answer: %w", err)
		}
	}
	return raw, nil
}

func printJSON(e *Env, raw []byte) {
	_, _ = e.Out.Write(bytes.TrimRight(raw, "\n"))
	_, _ = io.WriteString(e.Out, "\n")
}

func table(e *Env, head string, rows func(w io.Writer)) {
	tw := tabwriter.NewWriter(e.Out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, head)
	rows(tw)
	_ = tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func age(now time.Time, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// target is the DevPod's main container: persistence.targetContainer,
// else the first.
func target(dp *devpodv1alpha1.DevPod) *corev1.Container {
	if dp.Spec.Pod == nil || len(dp.Spec.Pod.Spec.Containers) == 0 {
		return nil
	}
	cs := dp.Spec.Pod.Spec.Containers
	if p := dp.Spec.Persistence; p != nil && p.TargetContainer != "" {
		for i := range cs {
			if cs[i].Name == p.TargetContainer {
				return &cs[i]
			}
		}
	}
	return &cs[0]
}

func limit(c *corev1.Container, name corev1.ResourceName) string {
	if c == nil {
		return ""
	}
	if q, ok := c.Resources.Limits[name]; ok {
		return q.String()
	}
	return ""
}

// gpuFlags reads --gpu in body: a number (of --gpu-type's kind, or the
// default), or several kinds at once — "nvidia=1,moore threads=4".
func gpuFlags(body map[string]any) error {
	spec, _ := body["gpu"].(string)
	if _, ok := body["gpuType"]; ok && spec == "" {
		return errors.New("--gpu-type goes with --gpu N")
	}
	if !strings.Contains(spec, "=") {
		return nil
	}
	if _, ok := body["gpuType"]; ok {
		return errors.New("--gpu kind=N,… names the kinds itself: leave --gpu-type out")
	}
	several := map[string]string{}
	for _, item := range strings.Split(spec, ",") {
		kind, n, ok := strings.Cut(strings.TrimSpace(item), "=")
		if kind, n = strings.TrimSpace(kind), strings.TrimSpace(n); !ok || kind == "" || n == "" {
			return fmt.Errorf("--gpu %q: N, or kind=N,kind=N", spec)
		}
		several[kind] = n
	}
	delete(body, "gpu")
	body["gpus"] = several
	return nil
}

// gpuLimit is a container's GPUs, of every vendor ("" for none).
func gpuLimit(c *corev1.Container) string {
	if c == nil {
		return ""
	}
	if n := gpures.Count(c.Resources.Limits); n > 0 {
		return strconv.Itoa(n)
	}
	return ""
}

func image(c *corev1.Container) string {
	if c == nil {
		return ""
	}
	return c.Image
}

type podInfo struct {
	Node string `json:"node"`
	IP   string `json:"ip"`
}

/* ---- commands ---- */

func runWhoami(e *Env, a *parsed) int {
	var me meView
	raw, err := call(e, "GET", "/api/me", nil, &me)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	fmt.Fprintf(e.Out, "user:     %s\nadmin:    %v\ndevpods:  %d (%d running)\n", me.User, me.Admin, me.Usage.DevPods, me.Usage.Running)
	usageLines := func(prefix string, u usageView, q map[string]any) {
		keys := make([]string, 0, len(u.Compute))
		for k := range u.Compute {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(e.Out, "%sin use:   %s %s\n", prefix, k, u.Compute[k])
		}
		if u.Storage != "" && u.Storage != "0" {
			fmt.Fprintf(e.Out, "%sstorage:  %s\n", prefix, u.Storage)
		}
		if len(q) > 0 {
			b, _ := json.Marshal(q)
			fmt.Fprintf(e.Out, "%squota:    %s\n", prefix, b)
		}
	}
	if len(me.Clusters) == 0 {
		usageLines("", me.Usage, me.Quota)
		return ExitOK
	}
	// quotas and usage are per cluster
	names := make([]string, 0, len(me.Clusters))
	for n := range me.Clusters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := me.Clusters[n]
		fmt.Fprintf(e.Out, "\ncluster %s: %d DevPods (%d running), ssh %s\n", n, c.Usage.DevPods, c.Usage.Running, me.sshOf(n).address())
		usageLines("  ", c.Usage, c.Quota)
	}
	return ExitOK
}

// listed is a DevPod as a list has it: with its cluster, in a console of
// several.
type listed struct {
	devpodv1alpha1.DevPod
	Cluster string `json:"cluster"`
}

func runList(e *Env, a *parsed) int {
	if len(a.pos) > 0 {
		return usage(e, find("list"), "list takes no names; use `show <name>`")
	}
	path := scoped(e, "", "/api/devpods")
	if a.has["all"] {
		path += "?scope=all"
	}
	var list struct {
		Items []listed           `json:"items"`
		Pods  map[string]podInfo `json:"pods"` // by <cluster>/<name> in a console of several
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
		fmt.Fprintln(e.Out, "no DevPods; create one with `create <name> --image IMG --cpu N --memory SIZE`")
		return ExitOK
	}
	several := false
	for _, it := range list.Items {
		several = several || it.Cluster != ""
	}
	sort.Slice(list.Items, func(i, j int) bool {
		if list.Items[i].Cluster != list.Items[j].Cluster {
			return list.Items[i].Cluster < list.Items[j].Cluster
		}
		return list.Items[i].Name < list.Items[j].Name
	})
	head := "NAME\tOWNER\tPHASE\tNODE\tIMAGE\tCPU\tMEMORY\tGPU\tAGE"
	if several {
		head = "CLUSTER\t" + head
	}
	table(e, head, func(w io.Writer) {
		for i := range list.Items {
			it := &list.Items[i]
			dp := &it.DevPod
			c := target(dp)
			if several {
				fmt.Fprintf(w, "%s\t", dash(it.Cluster))
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", dp.Name, dp.Spec.Owner, dash(string(dp.Status.Phase)), dash(list.Pods[joinRef(it.Cluster, dp.Name)].Node),
				dash(image(c)), dash(limit(c, corev1.ResourceCPU)), dash(limit(c, corev1.ResourceMemory)), dash(gpuLimit(c)),
				age(e.Now(), dp.CreationTimestamp.Time))
		}
	})
	return ExitOK
}

type detail struct {
	DevPod  devpodv1alpha1.DevPod `json:"devpod"`
	Cluster string                `json:"cluster"`
	Pod     *podInfo              `json:"pod"`
}

// podState is GET …/pod: the DevPod's Pod as Kubernetes has it.
type podState struct {
	Pod *struct {
		Phase      string `json:"phase"`
		Reason     string `json:"reason"`
		Ready      string `json:"ready"`
		Restarts   int32  `json:"restarts"`
		Containers []struct {
			Name      string `json:"name"`
			State     string `json:"state"`
			LastState string `json:"lastState"`
		} `json:"containers"`
	} `json:"pod"`
}

func runShow(e *Env, a *parsed) int {
	if len(a.pos) != 1 {
		return usage(e, find("show"), "show takes exactly one name")
	}
	var d detail
	raw, err := call(e, "GET", dpPath(e, a.pos[0]), nil, &d)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	dp, c := &d.DevPod, target(&d.DevPod)
	kv := [][2]string{
		{"name", dp.Name}, {"cluster", d.Cluster}, {"owner", dp.Spec.Owner}, {"phase", string(dp.Status.Phase)}, {"message", dp.Status.Message},
		{"running", strconv.FormatBool(dp.Spec.Running)},
	}
	if d.Pod != nil {
		kv = append(kv, [2]string{"node", d.Pod.Node}, [2]string{"ip", d.Pod.IP})
	}
	kv = append(kv, [2]string{"image", image(c)}, [2]string{"cpu", limit(c, corev1.ResourceCPU)}, [2]string{"memory", limit(c, corev1.ResourceMemory)},
		[2]string{"gpu", gpuLimit(c)}, [2]string{"shell", dp.Spec.Shell}, [2]string{"collaborators", strings.Join(dp.Spec.Collaborators, ", ")})
	if p := dp.Spec.Persistence; p != nil {
		kv = append(kv, [2]string{"home", p.Size.String() + " at " + p.MountPath})
	}
	if !dp.CreationTimestamp.IsZero() {
		kv = append(kv, [2]string{"created", dp.CreationTimestamp.UTC().Format(time.RFC3339)})
	}
	// the Pod as Kubernetes has it (what an empty phase does not say)
	var ps podState
	if _, err := call(e, "GET", dpPath(e, joinRef(d.Cluster, dp.Name))+"/pod", nil, &ps); err == nil && ps.Pod != nil {
		p := ps.Pod
		state := p.Phase
		if p.Reason != "" {
			state += " (" + p.Reason + ")"
		}
		kv = append(kv, [2]string{"pod", fmt.Sprintf("%s, %s ready, %d restarts", state, dash(p.Ready), p.Restarts)})
		for _, ct := range p.Containers {
			line := dash(ct.State)
			if ct.LastState != "" {
				line += "; last " + ct.LastState
			}
			kv = append(kv, [2]string{"container " + ct.Name, line})
		}
	}
	for _, r := range kv {
		if r[1] != "" {
			fmt.Fprintf(e.Out, "%-14s %s\n", r[0]+":", r[1])
		}
	}
	return ExitOK
}

func splitList(s string) []string {
	out := []string{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

func runCreate(e *Env, a *parsed) int {
	c := find("create")
	if len(a.pos) != 1 {
		return usage(e, c, "create takes exactly one name")
	}
	if a.val["image"] == "" {
		return usage(e, c, "give --image")
	}
	cluster, name := splitRef(a.pos[0])
	body := map[string]any{"name": name}
	for flag, field := range map[string]string{"image": "image", "cpu": "cpu", "memory": "memory", "gpu": "gpu", "gpu-type": "gpuType", "shell": "shell"} {
		if v, ok := a.val[flag]; ok {
			body[field] = v
		}
	}
	if err := gpuFlags(body); err != nil {
		return usage(e, c, err.Error())
	}
	if v, ok := a.val["collab"]; ok {
		body["collaborators"] = splitList(v)
	}
	var dp devpodv1alpha1.DevPod
	raw, err := call(e, "POST", scoped(e, cluster, "/api/devpods"), body, &dp)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	fmt.Fprintf(e.Out, "created %s\n", dp.Name)
	return ExitOK
}

type outcome struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
	note  string
}

// each runs fn for every name and reports per name; one failure does not
// stop the rest. JSON output is [{name, ok, code?, error?}].
func each(e *Env, a *parsed, c *command, done string, fn func(name string) (string, error)) int {
	if len(a.pos) == 0 {
		return usage(e, c, c.name+" needs at least one name")
	}
	res := make([]outcome, 0, len(a.pos))
	status := ExitOK
	for _, name := range a.pos {
		o := outcome{Name: name, OK: true}
		note, err := fn(name)
		o.note = note
		if err != nil {
			status, o.OK, o.Error = ExitAPI, false, err.Error()
			var ae *APIError
			if errors.As(err, &ae) {
				o.Code, o.Error = ae.Code, ae.Message
			}
			if !a.json {
				fmt.Fprintf(e.Err, "error: %s: %s\n", name, err)
			}
		} else if !a.json {
			fmt.Fprintf(e.Out, "%s: %s%s\n", name, done, note)
		}
		res = append(res, o)
	}
	if a.json {
		b, _ := json.Marshal(res)
		printJSON(e, b)
	}
	return status
}

type patchResult struct {
	DevPod  devpodv1alpha1.DevPod `json:"devpod"`
	Rebuild bool                  `json:"rebuild"`
}

func rebuildNote(r patchResult) string {
	if r.Rebuild {
		return " (the Pod is being rebuilt; sessions drop)"
	}
	return ""
}

func runRunning(on bool) func(e *Env, a *parsed) int {
	name, done := "stop", "stopped"
	if on {
		name, done = "start", "started"
	}
	return func(e *Env, a *parsed) int {
		return each(e, a, find(name), done, func(n string) (string, error) {
			_, err := call(e, "PATCH", dpPath(e, n), map[string]any{"running": on}, nil)
			return "", err
		})
	}
}

func runRedeploy(e *Env, a *parsed) int {
	return each(e, a, find("redeploy"), "redeploying", func(n string) (string, error) {
		_, err := call(e, "POST", dpPath(e, n)+"/redeploy", nil, nil)
		return "", err
	})
}

func runDelete(e *Env, a *parsed) int {
	c := find("delete")
	if len(a.pos) > 0 && !a.has["yes"] {
		return usage(e, c, "deleting removes the DevPod and its home volume; add --yes to confirm")
	}
	return each(e, a, c, "deleted", func(n string) (string, error) {
		_, err := call(e, "DELETE", dpPath(e, n), nil, nil)
		return "", err
	})
}

func runSet(e *Env, a *parsed) int {
	c := find("set")
	if len(a.pos) != 1 {
		return usage(e, c, "set takes exactly one name")
	}
	if len(a.val) == 0 {
		return usage(e, c, "nothing to change")
	}
	body := map[string]any{}
	for k, v := range a.val {
		if k == "gpu-type" {
			k = "gpuType"
		}
		body[k] = v
	}
	if err := gpuFlags(body); err != nil {
		return usage(e, c, err.Error())
	}
	var r patchResult
	raw, err := call(e, "PATCH", dpPath(e, a.pos[0]), body, &r)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	fmt.Fprintf(e.Out, "%s: updated%s\n", a.pos[0], rebuildNote(r))
	return ExitOK
}

func runCollab(e *Env, a *parsed) int {
	c := find("collab")
	if len(a.pos) == 0 {
		return usage(e, c, "collab needs a name")
	}
	name := a.pos[0]
	var d detail
	raw, err := call(e, "GET", dpPath(e, name), nil, &d)
	if err != nil {
		return fail(e, err)
	}
	cur := d.DevPod.Spec.Collaborators
	if len(a.pos) > 1 {
		users := a.pos[2:]
		var next []string
		switch a.pos[1] {
		case "add":
			next = slices.Clone(cur)
			for _, u := range users {
				if !slices.Contains(next, u) {
					next = append(next, u)
				}
			}
		case "remove", "rm":
			for _, u := range cur {
				if !slices.Contains(users, u) {
					next = append(next, u)
				}
			}
		case "set":
			next = users
		default:
			return usage(e, c, fmt.Sprintf("unknown action %q (add, remove or set)", a.pos[1]))
		}
		if a.pos[1] != "set" && len(users) == 0 {
			return usage(e, c, a.pos[1]+" needs at least one user")
		}
		if next == nil {
			next = []string{}
		}
		var r patchResult
		if raw, err = call(e, "PATCH", dpPath(e, name), map[string]any{"collaborators": next}, &r); err != nil {
			return fail(e, err)
		}
		cur = r.DevPod.Spec.Collaborators
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	if len(cur) == 0 {
		fmt.Fprintf(e.Out, "%s: no collaborators\n", name)
	} else {
		fmt.Fprintf(e.Out, "%s: %s\n", name, strings.Join(cur, ", "))
	}
	return ExitOK
}

type httpView struct {
	ID          string `json:"id"`
	URLTemplate string `json:"urlTemplate"`
	PublicPorts []int  `json:"publicPorts"`
	UserPorts   []int  `json:"userPorts"` // open to anyone signed in to the console
}

func runPorts(e *Env, a *parsed) int {
	c := find("ports")
	if len(a.pos) == 0 {
		return usage(e, c, "ports needs a name")
	}
	path := dpPath(e, a.pos[0]) + "/http"
	var v httpView
	raw, err := call(e, "GET", path, nil, &v)
	if err != nil {
		return fail(e, err)
	}
	ports := func(words []string) ([]int, error) {
		if len(words) == 0 {
			return nil, errors.New("give at least one port")
		}
		out := make([]int, 0, len(words))
		for _, w := range words {
			n, err := strconv.Atoi(w)
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("invalid port %q", w)
			}
			out = append(out, n)
		}
		return out, nil
	}
	if len(a.pos) > 1 {
		action, rest := a.pos[1], a.pos[2:]
		var body map[string]any
		switch action {
		case "url":
			ps, err := ports(rest)
			if err != nil || len(ps) != 1 {
				return usage(e, c, "url takes one port")
			}
			u := strings.Replace(v.URLTemplate, "{port}", strconv.Itoa(ps[0]), 1)
			if a.json {
				b, _ := json.Marshal(map[string]any{"url": u, "port": ps[0], "public": slices.Contains(v.PublicPorts, ps[0]), "access": portAccess(v.PublicPorts, v.UserPorts, ps[0])})
				printJSON(e, b)
			} else {
				fmt.Fprintln(e.Out, u)
			}
			return ExitOK
		case "public", "users", "private":
			// one access level per port: public (anyone with the link),
			// users (anyone signed in to the console), private (owner,
			// collaborators, admins)
			ps, err := ports(rest)
			if err != nil {
				return usage(e, c, err.Error())
			}
			without := func(list []int) []int {
				out := []int{}
				for _, p := range list {
					if !slices.Contains(ps, p) {
						out = append(out, p)
					}
				}
				return out
			}
			pub, users := without(v.PublicPorts), without(v.UserPorts)
			switch action {
			case "public":
				pub = append(pub, ps...)
			case "users":
				users = append(users, ps...)
			}
			body = map[string]any{"publicPorts": pub, "userPorts": users}
		case "reset":
			body = map[string]any{"reset": true}
		default:
			return usage(e, c, fmt.Sprintf("unknown action %q (url, public, users, private or reset)", action))
		}
		if raw, err = call(e, "PUT", path, body, &v); err != nil {
			return fail(e, err)
		}
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	fmt.Fprintf(e.Out, "address: %s\n", v.URLTemplate)
	list := func(ports []int) string {
		if len(ports) == 0 {
			return "none"
		}
		out := make([]string, len(ports))
		for i, p := range ports {
			out[i] = strconv.Itoa(p)
		}
		return strings.Join(out, ", ")
	}
	fmt.Fprintf(e.Out, "public:  %s (anyone with the link)\n", list(v.PublicPorts))
	fmt.Fprintf(e.Out, "users:   %s (anyone signed in to the console)\n", list(v.UserPorts))
	fmt.Fprintln(e.Out, "other ports: owner, collaborators and admins, after a console sign-in")
	return ExitOK
}

func runSSH(e *Env, a *parsed) int {
	if len(a.pos) != 1 {
		return usage(e, find("ssh"), "ssh takes exactly one name")
	}
	var d detail
	if _, err := call(e, "GET", dpPath(e, a.pos[0]), nil, &d); err != nil {
		return fail(e, err)
	}
	var me meView
	if _, err := call(e, "GET", "/api/me", nil, &me); err != nil {
		return fail(e, err)
	}
	ssh := me.sshOf(d.Cluster) // the gateway that serves the DevPod's cluster
	owner, name := d.DevPod.Spec.Owner, d.DevPod.Name
	pod := strings.TrimPrefix(name, owner+"-")
	login := owner + "+" + pod
	if ssh.LoginSuffix != "" {
		login += "+" + ssh.LoginSuffix
	}
	host, port := ssh.Host, ""
	if host == "" {
		host = "<gateway>"
	} else if ssh.Port != 0 && ssh.Port != 22 {
		port = fmt.Sprintf("-p %d ", ssh.Port)
	}
	cmd := fmt.Sprintf("ssh %s%s@%s", port, login, host)
	if a.json {
		b, _ := json.Marshal(map[string]string{"command": cmd, "login": login, "host": host})
		printJSON(e, b)
	} else {
		fmt.Fprintln(e.Out, cmd)
	}
	return ExitOK
}

func runNodes(e *Env, a *parsed) int {
	type res struct {
		Allocatable float64  `json:"allocatable"`
		Allocated   float64  `json:"allocated"`
		Used        *float64 `json:"used"`
	}
	var list struct {
		Items []struct {
			Cluster string `json:"cluster"`
			Name    string `json:"name"`
			Ready   bool   `json:"ready"`
			CPU     res    `json:"cpu"`
			Mem     res    `json:"mem"`
			GPU     struct {
				Model string `json:"model"`
				Total int    `json:"total"`
				Used  int    `json:"used"`
			} `json:"gpu"`
			DevPods int `json:"devpods"`
		} `json:"items"`
	}
	raw, err := call(e, "GET", scoped(e, "", "/api/admin/nodes"), nil, &list)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	gib := func(b float64) string { return strconv.FormatFloat(b/(1<<30), 'f', 0, 64) + "Gi" }
	several := false
	for _, n := range list.Items {
		several = several || n.Cluster != ""
	}
	head := "NAME\tREADY\tCPU alloc/total\tMEMORY alloc/total\tGPU used/total\tDEVPODS"
	if several {
		head = "CLUSTER\t" + head
	}
	table(e, head, func(w io.Writer) {
		for _, n := range list.Items {
			if several {
				fmt.Fprintf(w, "%s\t", dash(n.Cluster))
			}
			gpu := "-"
			if n.GPU.Total > 0 {
				gpu = fmt.Sprintf("%d/%d %s", n.GPU.Used, n.GPU.Total, n.GPU.Model)
			}
			fmt.Fprintf(w, "%s\t%v\t%s/%s\t%s/%s\t%s\t%d\n", n.Name, n.Ready,
				strconv.FormatFloat(n.CPU.Allocated, 'f', -1, 64), strconv.FormatFloat(n.CPU.Allocatable, 'f', -1, 64),
				gib(n.Mem.Allocated), gib(n.Mem.Allocatable), strings.TrimSpace(gpu), n.DevPods)
		}
	})
	return ExitOK
}

func runUsers(e *Env, a *parsed) int {
	var list struct {
		Items []struct {
			Name    string `json:"name"`
			Admin   bool   `json:"admin"`
			DevPods int    `json:"devpods"`
			Running int    `json:"running"`
			Source  string `json:"source"`
			Pubkeys int    `json:"pubkeys"`
		} `json:"items"`
	}
	raw, err := call(e, "GET", "/api/admin/users", nil, &list)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	table(e, "NAME\tADMIN\tDEVPODS\tRUNNING\tSOURCE\tKEYS", func(w io.Writer) {
		for _, u := range list.Items {
			fmt.Fprintf(w, "%s\t%v\t%d\t%d\t%s\t%d\n", u.Name, u.Admin, u.DevPods, u.Running, dash(u.Source), u.Pubkeys)
		}
	})
	return ExitOK
}

func runMigrate(e *Env, a *parsed) int {
	if len(a.pos) != 2 {
		return usage(e, find("migrate"), "migrate takes a name and a node (or - to unpin)")
	}
	node := a.pos[1]
	if node == "-" {
		node = ""
	}
	var r patchResult
	raw, err := call(e, "PATCH", dpPath(e, a.pos[0]), map[string]any{"node": node}, &r)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	if node == "" {
		fmt.Fprintf(e.Out, "%s: unpinned%s\n", a.pos[0], rebuildNote(r))
	} else {
		fmt.Fprintf(e.Out, "%s: pinned to %s%s\n", a.pos[0], node, rebuildNote(r))
	}
	return ExitOK
}

// portAccess is a port's access level; public wins if it is listed twice.
func portAccess(public, users []int, port int) string {
	switch {
	case slices.Contains(public, port):
		return "public"
	case slices.Contains(users, port):
		return "users"
	}
	return "private"
}
