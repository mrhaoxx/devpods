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
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
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
}

type parsed struct {
	pos  []string
	val  map[string]string
	has  map[string]bool
	json bool
}

// command is one row of the table the dispatcher, the parser and the
// help text are all generated from.
type command struct {
	name    string
	aliases []string
	args    string   // the argument synopsis
	short   string   // one line for the list
	long    string   // more for `help <command>`
	admin   bool     // the API only answers admins
	vals    []string // flags that take a value
	bools   []string // flags that do not
	run     func(e *Env, a *parsed) int
}

var commands []*command

func init() {
	edit := []string{"image", "cpu", "memory", "gpu", "shell"}
	commands = []*command{
		{name: "help", args: "[command]", short: "show this help, or one command's", run: nil},
		{name: "whoami", short: "who you are, your quota and usage", run: runWhoami},
		{name: "list", aliases: []string{"ls"}, args: "[--all]", short: "list your DevPods (--all: everyone's, admin)", bools: []string{"all"}, run: runList},
		{name: "show", aliases: []string{"status", "get"}, args: "<name>", short: "one DevPod in detail", run: runShow},
		{name: "create", args: "<name> --image IMG [--cpu N] [--memory SIZE] [--gpu N] [--template T] [--shell S] [--collab a,b]",
			short: "create a DevPod",
			long:  "<name> is the part after your user name: `create dev1` makes <you>-dev1.\nNon-admins must give --cpu and --memory (or a template that sets them).",
			vals:  append([]string{"template", "collab"}, edit...), run: runCreate},
		{name: "start", args: "<name>…", short: "start (wake) DevPods", run: runRunning(true)},
		{name: "stop", args: "<name>…", short: "stop (hibernate) DevPods; the home volume stays", run: runRunning(false)},
		{name: "redeploy", args: "<name>…", short: "recreate the Pod (sessions drop, the home volume stays)", run: runRedeploy},
		{name: "delete", aliases: []string{"rm"}, args: "<name>… --yes", short: "delete DevPods and their data", bools: []string{"yes"}, run: runDelete},
		{name: "set", args: "<name> [--image IMG] [--cpu N] [--memory SIZE] [--gpu N] [--shell S]", short: "change a DevPod (a running one is rebuilt)", vals: edit, run: runSet},
		{name: "collab", args: "<name> [add|remove|set user…]", short: "show or change collaborators", run: runCollab},
		{name: "ports", args: "<name> [url <port> | public <port>… | private <port>… | reset]", short: "HTTP addresses of a DevPod's ports",
			long: "Without a subcommand: the address pattern and the public ports.\nurl prints one port's address; public / private open or close a port to\npeople who are not signed in; reset draws a new random address (old links die).",
			run:  runPorts},
		{name: "ssh", args: "<name>", short: "print the ssh command for a DevPod", run: runSSH},
		{name: "templates", short: "list DevPod templates", run: runTemplates},
		{name: "nodes", short: "list nodes", admin: true, run: runNodes},
		{name: "users", short: "list users", admin: true, run: runUsers},
		{name: "migrate", args: "<name> <node|->", short: "pin a DevPod to a node (- lets the scheduler choose)", admin: true, run: runMigrate},
	}
	commands = append(commands, snapshotCommands()...)
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
func Help(w io.Writer, name string) bool {
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
		if len(c.aliases) > 0 {
			fmt.Fprintf(w, "\naliases: %s\n", strings.Join(c.aliases, ", "))
		}
		return true
	}
	fmt.Fprintln(w, "DevPod CLI — usage: ssh devpod@<gateway> <command> [arguments]")
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, c := range commands {
		short := c.short
		if c.admin {
			short += " (admin)"
		}
		fmt.Fprintf(tw, "  %s\t%s\n", c.synopsis(), short)
	}
	_ = tw.Flush()
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Every command takes --json (or -o json): the API's JSON on stdout, for scripts and agents.")
	fmt.Fprintln(w, "Exit status: 0 ok, 1 the API refused, 2 bad command line. Errors go to stderr.")
	return true
}

func parse(c *command, argv []string) (*parsed, error) {
	a := &parsed{val: map[string]string{}, has: map[string]bool{}}
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
		Help(e.Out, "")
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
		if !Help(e.Out, topic) {
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
		Help(e.Out, c.name)
		return ExitOK
	}
	a, err := parse(c, rest)
	if err != nil {
		return usage(e, c, err.Error())
	}
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
		return nil, err
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

func dpPath(name string) string { return "/api/devpods/" + url.PathEscape(name) }

/* ---- commands ---- */

func runWhoami(e *Env, a *parsed) int {
	var me struct {
		User  string `json:"user"`
		Admin bool   `json:"admin"`
		Usage struct {
			DevPods int               `json:"devpods"`
			Running int               `json:"running"`
			Compute map[string]string `json:"compute"`
			Storage string            `json:"storage"`
		} `json:"usage"`
		Quota map[string]any `json:"quota"`
	}
	raw, err := call(e, "GET", "/api/me", nil, &me)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	fmt.Fprintf(e.Out, "user:     %s\nadmin:    %v\ndevpods:  %d (%d running)\n", me.User, me.Admin, me.Usage.DevPods, me.Usage.Running)
	keys := make([]string, 0, len(me.Usage.Compute))
	for k := range me.Usage.Compute {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(e.Out, "in use:   %s %s\n", k, me.Usage.Compute[k])
	}
	if me.Usage.Storage != "" && me.Usage.Storage != "0" {
		fmt.Fprintf(e.Out, "storage:  %s\n", me.Usage.Storage)
	}
	if len(me.Quota) > 0 {
		q, _ := json.Marshal(me.Quota)
		fmt.Fprintf(e.Out, "quota:    %s\n", q)
	}
	return ExitOK
}

func runList(e *Env, a *parsed) int {
	if len(a.pos) > 0 {
		return usage(e, find("list"), "list takes no names; use `show <name>`")
	}
	path := "/api/devpods"
	if a.has["all"] {
		path += "?scope=all"
	}
	var list struct {
		Items []devpodv1alpha1.DevPod `json:"items"`
		Pods  map[string]podInfo      `json:"pods"`
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
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	table(e, "NAME\tOWNER\tPHASE\tNODE\tIMAGE\tCPU\tMEMORY\tGPU\tAGE", func(w io.Writer) {
		for i := range list.Items {
			dp := &list.Items[i]
			c := target(dp)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", dp.Name, dp.Spec.Owner, dash(string(dp.Status.Phase)), dash(list.Pods[dp.Name].Node),
				dash(image(c)), dash(limit(c, corev1.ResourceCPU)), dash(limit(c, corev1.ResourceMemory)), dash(limit(c, "nvidia.com/gpu")),
				age(e.Now(), dp.CreationTimestamp.Time))
		}
	})
	return ExitOK
}

type detail struct {
	DevPod devpodv1alpha1.DevPod `json:"devpod"`
	Pod    *podInfo              `json:"pod"`
}

func runShow(e *Env, a *parsed) int {
	if len(a.pos) != 1 {
		return usage(e, find("show"), "show takes exactly one name")
	}
	var d detail
	raw, err := call(e, "GET", dpPath(a.pos[0]), nil, &d)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	dp, c := &d.DevPod, target(&d.DevPod)
	kv := [][2]string{
		{"name", dp.Name}, {"owner", dp.Spec.Owner}, {"phase", string(dp.Status.Phase)}, {"message", dp.Status.Message},
		{"running", strconv.FormatBool(dp.Spec.Running)},
	}
	if d.Pod != nil {
		kv = append(kv, [2]string{"node", d.Pod.Node}, [2]string{"ip", d.Pod.IP})
	}
	kv = append(kv, [2]string{"image", image(c)}, [2]string{"cpu", limit(c, corev1.ResourceCPU)}, [2]string{"memory", limit(c, corev1.ResourceMemory)},
		[2]string{"gpu", limit(c, "nvidia.com/gpu")}, [2]string{"shell", dp.Spec.Shell}, [2]string{"collaborators", strings.Join(dp.Spec.Collaborators, ", ")})
	if p := dp.Spec.Persistence; p != nil {
		kv = append(kv, [2]string{"home", p.Size.String() + " at " + p.MountPath})
	}
	if !dp.CreationTimestamp.IsZero() {
		kv = append(kv, [2]string{"created", dp.CreationTimestamp.UTC().Format(time.RFC3339)})
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
	if a.val["image"] == "" && a.val["template"] == "" {
		return usage(e, c, "give --image (or --template)")
	}
	body := map[string]any{"name": a.pos[0]}
	for flag, field := range map[string]string{"image": "image", "cpu": "cpu", "memory": "memory", "gpu": "gpu", "shell": "shell", "template": "templateRef"} {
		if v, ok := a.val[flag]; ok {
			body[field] = v
		}
	}
	if v, ok := a.val["collab"]; ok {
		body["collaborators"] = splitList(v)
	}
	var dp devpodv1alpha1.DevPod
	raw, err := call(e, "POST", "/api/devpods", body, &dp)
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
			_, err := call(e, "PATCH", dpPath(n), map[string]any{"running": on}, nil)
			return "", err
		})
	}
}

func runRedeploy(e *Env, a *parsed) int {
	return each(e, a, find("redeploy"), "redeploying", func(n string) (string, error) {
		_, err := call(e, "POST", dpPath(n)+"/redeploy", nil, nil)
		return "", err
	})
}

func runDelete(e *Env, a *parsed) int {
	c := find("delete")
	if len(a.pos) > 0 && !a.has["yes"] {
		return usage(e, c, "deleting removes the DevPod and its home volume; add --yes to confirm")
	}
	return each(e, a, c, "deleted", func(n string) (string, error) {
		_, err := call(e, "DELETE", dpPath(n), nil, nil)
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
		body[k] = v
	}
	var r patchResult
	raw, err := call(e, "PATCH", dpPath(a.pos[0]), body, &r)
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
	raw, err := call(e, "GET", dpPath(name), nil, &d)
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
		if raw, err = call(e, "PATCH", dpPath(name), map[string]any{"collaborators": next}, &r); err != nil {
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
}

func runPorts(e *Env, a *parsed) int {
	c := find("ports")
	if len(a.pos) == 0 {
		return usage(e, c, "ports needs a name")
	}
	path := dpPath(a.pos[0]) + "/http"
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
				b, _ := json.Marshal(map[string]any{"url": u, "port": ps[0], "public": slices.Contains(v.PublicPorts, ps[0])})
				printJSON(e, b)
			} else {
				fmt.Fprintln(e.Out, u)
			}
			return ExitOK
		case "public", "private":
			ps, err := ports(rest)
			if err != nil {
				return usage(e, c, err.Error())
			}
			next := []int{}
			for _, p := range v.PublicPorts {
				if action == "public" || !slices.Contains(ps, p) {
					next = append(next, p)
				}
			}
			if action == "public" {
				for _, p := range ps {
					if !slices.Contains(next, p) {
						next = append(next, p)
					}
				}
			}
			body = map[string]any{"publicPorts": next}
		case "reset":
			body = map[string]any{"reset": true}
		default:
			return usage(e, c, fmt.Sprintf("unknown action %q (url, public, private or reset)", action))
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
	if len(v.PublicPorts) == 0 {
		fmt.Fprintln(e.Out, "public:  none (every port needs a console sign-in: owner, collaborators, admins)")
	} else {
		s := make([]string, len(v.PublicPorts))
		for i, p := range v.PublicPorts {
			s[i] = strconv.Itoa(p)
		}
		fmt.Fprintf(e.Out, "public:  %s\n", strings.Join(s, ", "))
	}
	return ExitOK
}

func runSSH(e *Env, a *parsed) int {
	if len(a.pos) != 1 {
		return usage(e, find("ssh"), "ssh takes exactly one name")
	}
	var d detail
	if _, err := call(e, "GET", dpPath(a.pos[0]), nil, &d); err != nil {
		return fail(e, err)
	}
	var me struct {
		SSH struct {
			Host        string `json:"host"`
			Port        int    `json:"port"`
			LoginSuffix string `json:"loginSuffix"`
		} `json:"ssh"`
	}
	if _, err := call(e, "GET", "/api/me", nil, &me); err != nil {
		return fail(e, err)
	}
	owner, name := d.DevPod.Spec.Owner, d.DevPod.Name
	pod := strings.TrimPrefix(name, owner+"-")
	login := owner + "+" + pod
	if me.SSH.LoginSuffix != "" {
		login += "+" + me.SSH.LoginSuffix
	}
	host, port := me.SSH.Host, ""
	if host == "" {
		host = "<gateway>"
	} else if me.SSH.Port != 0 && me.SSH.Port != 22 {
		port = fmt.Sprintf("-p %d ", me.SSH.Port)
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

func runTemplates(e *Env, a *parsed) int {
	var list struct {
		Items []devpodv1alpha1.DevPodTemplate `json:"items"`
	}
	raw, err := call(e, "GET", "/api/templates", nil, &list)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(e.Out, "no templates")
		return ExitOK
	}
	for _, t := range list.Items {
		fmt.Fprintln(e.Out, t.Name)
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
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
			CPU   res    `json:"cpu"`
			Mem   res    `json:"mem"`
			GPU   struct {
				Model string `json:"model"`
				Total int    `json:"total"`
				Used  int    `json:"used"`
			} `json:"gpu"`
			DevPods int `json:"devpods"`
		} `json:"items"`
	}
	raw, err := call(e, "GET", "/api/admin/nodes", nil, &list)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	gib := func(b float64) string { return strconv.FormatFloat(b/(1<<30), 'f', 0, 64) + "Gi" }
	table(e, "NAME\tREADY\tCPU alloc/total\tMEMORY alloc/total\tGPU used/total\tDEVPODS", func(w io.Writer) {
		for _, n := range list.Items {
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
	raw, err := call(e, "PATCH", dpPath(a.pos[0]), map[string]any{"node": node}, &r)
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
