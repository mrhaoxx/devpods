// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// Snapshots (snapshot v2 spec §2.3–2.5): save a DevPod's container as an
// image, watch it happen, cancel it.

// ExitInterrupted: the user interrupted the command (Ctrl-C, a signal,
// or the connection closing).
const ExitInterrupted = 130

const (
	followIdle   = 60 * time.Second // no event for this long: stop following
	followBudget = 60 * time.Minute // the longest one follow lasts
)

// snapshot's sub-verbs; any other first word is a DevPod to snapshot.
var snapshotVerbs = []string{"create", "show", "follow", "retry", "cancel", "rm"}

const snapshotLong = `snapshot <devpod> saves the DevPod's container as an image and follows the
progress until it ends. The container is paused while it is committed
(--no-pause skips that; the image may then be inconsistent). Volumes — the
home directory — are not part of the image.

While it follows, Ctrl-C CANCELS the snapshot, and so does disconnecting.
Use --detach to start it and return at once; nothing is cancelled then.
"snapshot follow <id>" re-attaches; there Ctrl-C only stops following.

The image goes to your default address unless --image names another
(registry/path:tag). Another address needs its own credentials, unless
you saved a login for that registry ("registry add <host>"): give
--username; the password is asked for without echo on a terminal, or read
from the first line of standard input (--password-stdin forces that). The
password never goes on the command line.

"snapshot retry <id>" runs a failed snapshot again.

The words create, show, follow, retry, cancel and rm are sub-commands; a
DevPod with one of those names is reached with "snapshot create <devpod>".
With several clusters a snapshot's id is <cluster>/<id>, as the lists show it.

With --json the progress is one JSON object per line.`

func snapshotCommands() []*command {
	return []*command{
		{group: "Images", name: "snapshot", args: "<devpod> [--name N] [--tag T | --new-tag] [--image REF [--username U] [--password-stdin]] [--no-pause] [--detach] | show <id> | follow <id> | retry <id> | cancel <id> | rm <id>",
			brief: "<devpod> [flags] | show|follow|retry|cancel|rm <id>",
			short: "save a DevPod as an image and follow it (Ctrl-C cancels; --detach)",
			long:  snapshotLong,
			vals:  []string{"name", "tag", "image", "username"}, bools: []string{"no-pause", "detach", "password-stdin", "new-tag"}, run: runSnapshot},
		{name: "snapshots", aliases: []string{"images"}, args: "[--all] [--devpod D] [--cluster C]", short: "list snapshots (--all: everyone's, admin)",
			bools: []string{"all"}, vals: []string{"devpod"}, run: runSnapshots},
	}
}

type snapView struct {
	Cluster     string     `json:"cluster"`
	ID          string     `json:"id"`
	DevPod      string     `json:"devpod"`
	Owner       string     `json:"owner"`
	Image       string     `json:"image"`
	Phase       string     `json:"phase"`
	Digest      string     `json:"digest"`
	SizeBytes   int64      `json:"sizeBytes"`
	Message     string     `json:"message"`
	CreatedAt   *time.Time `json:"createdAt"`
	StartedAt   *time.Time `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt"`
}

func (v *snapView) finished() bool { return v.Phase == "Succeeded" || v.Phase == "Failed" }

type snapProgress struct {
	Phase          string   `json:"phase"`
	Stage          string   `json:"stage"`
	ElapsedSeconds float64  `json:"elapsedSeconds"`
	SizeBytes      int64    `json:"sizeBytes"`
	PushedBytes    int64    `json:"pushedBytes"`
	TotalBytes     int64    `json:"totalBytes"`
	Digest         string   `json:"digest"`
	Message        string   `json:"message"`
	Lines          []string `json:"lines"`
}

func (p *snapProgress) done() bool   { return p.Stage == "done" || p.Phase == "Succeeded" }
func (p *snapProgress) failed() bool { return p.Stage == "error" || p.Phase == "Failed" }

type pushAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type snapRequest struct {
	Name     string    `json:"name,omitempty"`
	Tag      string    `json:"tag,omitempty"`
	Pause    *bool     `json:"pause,omitempty"`
	Image    string    `json:"image,omitempty"`
	PushAuth *pushAuth `json:"pushAuth,omitempty"`
}

// snapPath is a snapshot's API path; ref is <id> or <cluster>/<id>.
func snapPath(e *Env, ref string) string {
	c, id := splitRef(ref)
	return scoped(e, c, "/api/snapshots/"+url.PathEscape(id))
}

var stageLabel = map[string]string{"pending": "等待开始", "commit": "提交容器", "push": "推送镜像", "done": "完成", "error": "失败"}

func fmtSize(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func fmtElapsed(sec float64) string {
	s := int(sec)
	if s >= 3600 {
		return fmt.Sprintf("%dh%02dm%02ds", s/3600, s%3600/60, s%60)
	}
	if s >= 60 {
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%ds", s)
}

// printable drops everything that could drive a terminal: log lines come
// from tools running against a user's container.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// newLines: the progress carries the tail of the log each time; what is
// new is what follows the longest overlap with the tail seen before.
func newLines(prev, cur []string) []string {
	for k := min(len(prev), len(cur)); k > 0; k-- {
		same := true
		for i := 0; i < k; i++ {
			if prev[len(prev)-k+i] != cur[i] {
				same = false
				break
			}
		}
		if same {
			if k == len(cur) {
				return nil
			}
			return cur[k:]
		}
	}
	if len(cur) == 0 {
		return nil
	}
	return cur
}

func runSnapshot(e *Env, a *parsed) int {
	c := find("snapshot")
	if len(a.pos) == 0 {
		return usage(e, c, "snapshot needs a DevPod, or one of: "+strings.Join(snapshotVerbs, ", "))
	}
	verb, rest := "create", a.pos
	for _, v := range snapshotVerbs {
		if a.pos[0] == v {
			verb, rest = v, a.pos[1:]
		}
	}
	if len(rest) != 1 {
		what := "one snapshot id"
		if verb == "create" {
			what = "one DevPod"
		}
		return usage(e, c, fmt.Sprintf("snapshot %s takes exactly %s", verb, what))
	}
	if verb != "create" {
		for _, f := range []string{"name", "tag", "image", "username"} {
			if _, ok := a.val[f]; ok {
				return usage(e, c, fmt.Sprintf("--%s only applies when creating a snapshot", f))
			}
		}
	}
	switch verb {
	case "create":
		return snapCreate(e, a, c, rest[0])
	case "show":
		return snapShow(e, a, rest[0])
	case "follow":
		var v snapView
		if _, err := call(e, "GET", snapPath(e, rest[0]), nil, &v); err != nil {
			return fail(e, err)
		}
		v.ID = rest[0] // as given: <id> or <cluster>/<id>
		return snapFollow(e, a, &v, false)
	case "retry":
		var v snapView
		raw, err := call(e, "POST", snapPath(e, rest[0])+"/retry", nil, &v)
		if err != nil {
			return fail(e, err)
		}
		if a.json {
			printJSON(e, raw)
			return ExitOK
		}
		fmt.Fprintf(e.Out, "snapshot %s is being taken again; follow it with `snapshot follow %s`\n", rest[0], rest[0])
		return ExitOK
	case "cancel":
		return snapEnd(e, rest[0], true)
	default:
		return snapEnd(e, rest[0], false)
	}
}

// credentials asks for what a custom image address needs. The password
// comes from the terminal without echo, or from standard input; never
// from the command line.
func credentials(e *Env, a *parsed) (*pushAuth, error) {
	user := a.val["username"]
	if a.has["password-stdin"] || e.ReadLine == nil {
		if user == "" {
			return nil, errors.New("--username is needed with a password from standard input")
		}
		if e.StdinLine == nil {
			return nil, errors.New("no password: give it on the first line of standard input")
		}
		pw, err := e.StdinLine()
		if err != nil || pw == "" {
			return nil, errors.New("no password on standard input (the first line is the password)")
		}
		return &pushAuth{Username: user, Password: pw}, nil
	}
	var err error
	if user == "" {
		if user, err = e.ReadLine("仓库用户名: ", true); err != nil {
			return nil, err
		}
		if user = strings.TrimSpace(user); user == "" {
			return nil, errors.New("no username given")
		}
	}
	pw, err := e.ReadLine("仓库密码（不回显）: ", false)
	if err != nil {
		return nil, err
	}
	if pw == "" {
		return nil, errors.New("no password given")
	}
	return &pushAuth{Username: user, Password: pw}, nil
}

func snapCreate(e *Env, a *parsed, c *command, devpod string) int {
	req := snapRequest{Name: a.val["name"], Tag: a.val["tag"], Image: a.val["image"]}
	_, hasUser := a.val["username"]
	if (hasUser || a.has["password-stdin"]) && req.Image == "" {
		return usage(e, c, "--username and --password-stdin go with --image (your default address needs no credentials)")
	}
	if req.Image == "" && req.Name == "" {
		// the DevPod's own name, without the caller's prefix
		req.Name = strings.TrimPrefix(devpod, e.User+"-")
	}
	// Saving to an address again overwrites that tag. --new-tag stamps the
	// address with the date and time instead (made here, not by the API),
	// so every save is a new image.
	if a.has["new-tag"] {
		if req.Tag != "" {
			return usage(e, c, "--tag and --new-tag exclude each other")
		}
		stamp := e.Now().In(shanghai).Format("20060102-150405")
		if req.Image != "" {
			req.Image = withTag(req.Image, stamp)
		} else {
			req.Tag = stamp
		}
	}
	if a.has["no-pause"] {
		f := false
		req.Pause = &f
	}
	if hasUser || a.has["password-stdin"] {
		auth, err := credentials(e, a)
		if err != nil {
			if errors.Is(err, errInterrupted) {
				return ExitInterrupted
			}
			return usage(e, c, err.Error())
		}
		req.PushAuth = auth
	}
	var v snapView
	path := dpPath(e, devpod) + "/snapshots"
	raw, err := call(e, "POST", path, req, &v)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == "PUSH_AUTH_REQUIRED" && req.PushAuth == nil {
		if e.ReadLine == nil {
			fmt.Fprintf(e.Err, "error: pushing to %s needs that registry's credentials: add --username U and give the password on the first line of standard input, or save a login once with `registry add %s` (PUSH_AUTH_REQUIRED)\n", req.Image, imageRegistry(req.Image))
			return ExitAPI
		}
		fmt.Fprintf(e.Err, "%s is not your default address: it needs that registry's credentials.\n(`registry add %s` saves a login so this is not asked again.)\n", req.Image, imageRegistry(req.Image))
		auth, cerr := credentials(e, a)
		if cerr != nil {
			if errors.Is(cerr, errInterrupted) {
				return ExitInterrupted
			}
			return usage(e, c, cerr.Error())
		}
		req.PushAuth = auth
		raw, err = call(e, "POST", path, req, &v) // once more, with credentials
	}
	if err != nil {
		return fail(e, err)
	}
	// from here on its id says its cluster (the DevPod's)
	if v.Cluster == "" {
		v.Cluster, _ = splitRef(devpod)
		if v.Cluster == "" {
			v.Cluster = e.Cluster
		}
	}
	v.ID = joinRef(v.Cluster, v.ID)
	if a.has["detach"] {
		if a.json {
			printJSON(e, raw)
			return ExitOK
		}
		fmt.Fprintf(e.Out, "snapshot %s started: %s → %s\nfollow it with `snapshot follow %s`; cancel with `snapshot cancel %s`\n", v.ID, devpod, v.Image, v.ID, v.ID)
		return ExitOK
	}
	return snapFollow(e, a, &v, true)
}

func snapShow(e *Env, a *parsed, id string) int {
	var v snapView
	raw, err := call(e, "GET", snapPath(e, id), nil, &v)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	ts := func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	size := ""
	if v.SizeBytes > 0 {
		size = fmtSize(v.SizeBytes)
	}
	for _, r := range [][2]string{{"id", v.ID}, {"devpod", v.DevPod}, {"owner", v.Owner}, {"image", v.Image}, {"status", v.Phase}, {"message", v.Message},
		{"digest", v.Digest}, {"size", size}, {"created", ts(v.CreatedAt)}, {"started", ts(v.StartedAt)}, {"completed", ts(v.CompletedAt)}} {
		if r[1] != "" {
			fmt.Fprintf(e.Out, "%-11s %s\n", r[0]+":", r[1])
		}
	}
	return ExitOK
}

// snapEnd cancels a snapshot in progress or removes a finished one's
// record. The API has one DELETE for both, so the state is checked
// first: cancel must not delete a record, rm must not cancel.
func snapEnd(e *Env, id string, cancel bool) int {
	var v snapView
	if _, err := call(e, "GET", snapPath(e, id), nil, &v); err != nil {
		return fail(e, err)
	}
	if cancel && v.finished() {
		fmt.Fprintf(e.Err, "error: snapshot %s has already finished (%s); remove its record with `snapshot rm %s`\n", id, v.Phase, id)
		return ExitAPI
	}
	if !cancel && !v.finished() {
		fmt.Fprintf(e.Err, "error: snapshot %s is still in progress; stop it with `snapshot cancel %s`\n", id, id)
		return ExitAPI
	}
	if _, err := call(e, "DELETE", snapPath(e, id), nil, nil); err != nil {
		return fail(e, err)
	}
	if cancel {
		fmt.Fprintln(e.Out, "已取消")
	} else {
		fmt.Fprintf(e.Out, "removed the record of snapshot %s (the image stays in the registry)\n", id)
	}
	return ExitOK
}

// snapFollow shows a snapshot's progress until it ends. With cancel an
// interrupt (Ctrl-C, a signal, the connection closing) cancels the
// snapshot; without, it only stops following.
func snapFollow(e *Env, a *parsed, v *snapView, cancel bool) int {
	base := e.Session
	if base == nil {
		base = e.Ctx
	}
	budget, idle := e.FollowBudget, e.FollowIdle
	if budget <= 0 {
		budget = followBudget
	}
	if idle <= 0 {
		idle = followIdle
	}
	ctx, stop := context.WithTimeout(base, budget)
	defer stop()
	var interrupted atomic.Bool
	go func() {
		select {
		case <-e.Interrupt: // a nil channel never fires
			interrupted.Store(true)
			stop()
		case <-ctx.Done():
		}
	}()

	r := &snapRenderer{e: e, json: a.json, tty: e.TTY && !a.json, id: v.ID, image: v.Image}
	if !a.json {
		fmt.Fprintf(e.Out, "快照 %s → %s\n", v.ID, v.Image)
	}
	var last *snapProgress
	err := e.API.Stream(ctx, e.User, snapPath(e, v.ID)+"/progress", idle, func(data []byte) error {
		var p snapProgress
		if json.Unmarshal(data, &p) != nil {
			return nil // not a progress object: skip it
		}
		r.event(&p, data)
		last = &p
		if p.done() || p.failed() {
			return errStreamStop
		}
		return nil
	})
	r.clear()

	if interrupted.Load() {
		if !cancel {
			fmt.Fprintf(e.Err, "stopped following; the snapshot goes on. Re-attach with `snapshot follow %s`, cancel with `snapshot cancel %s`\n", v.ID, v.ID)
			return ExitInterrupted
		}
		// the session may be gone: the cancel runs on its own short clock
		cctx, done := context.WithTimeout(context.WithoutCancel(base), 10*time.Second)
		defer done()
		if _, derr := e.API.Do(cctx, e.User, "DELETE", snapPath(e, v.ID), nil); derr != nil {
			fmt.Fprintf(e.Err, "error: cancelling snapshot %s failed: %s\n", v.ID, derr)
			return ExitAPI
		}
		fmt.Fprintln(e.Out, "已取消")
		return ExitInterrupted
	}
	switch {
	case last != nil && last.done():
		if !a.json {
			ref := v.Image
			if last.Digest != "" {
				ref += "@" + last.Digest
			}
			fmt.Fprintf(e.Out, "完成：%s（%s，%s）\n", ref, fmtSize(last.SizeBytes), fmtElapsed(last.ElapsedSeconds))
		}
		return ExitOK
	case last != nil && last.failed():
		msg := last.Message
		if msg == "" {
			msg = "the snapshot failed"
		}
		fmt.Fprintf(e.Err, "失败：%s\n", printable(msg))
		return ExitAPI
	}
	var ae *APIError
	if errors.As(err, &ae) && ae.Status != 0 {
		return fail(e, err)
	}
	why := "the progress stream ended"
	switch {
	case errors.Is(err, ErrStreamIdle):
		why = fmt.Sprintf("no progress for %s", idle)
	case errors.Is(err, context.DeadlineExceeded):
		why = fmt.Sprintf("followed for %s", budget)
	case err != nil:
		why = err.Error()
	}
	fmt.Fprintf(e.Err, "error: %s; the snapshot itself goes on. Re-attach with `snapshot follow %s`\n", why, v.ID)
	return ExitAPI
}

// snapRenderer prints progress: on a terminal one status line redrawn in
// place with new log lines above it; otherwise appended lines only.
type snapRenderer struct {
	e         *Env
	json, tty bool
	id, image string
	prev      []string
	stage     string
	status    bool // a status line is on screen
}

const eraseLine = "\r\x1b[K"

func (r *snapRenderer) clear() {
	if r.status {
		_, _ = io.WriteString(r.e.Out, eraseLine)
		r.status = false
	}
}

func (r *snapRenderer) event(p *snapProgress, raw []byte) {
	if r.json {
		// the progress object as sent, plus which snapshot it is about
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		m["id"], m["image"] = r.id, r.image
		if b, err := json.Marshal(m); err == nil {
			_, _ = r.e.Out.Write(append(b, '\n'))
		}
		return
	}
	fresh := newLines(r.prev, p.Lines)
	r.prev = p.Lines
	label := stageLabel[p.Stage]
	if label == "" {
		label = p.Stage
	}
	final := p.done() || p.failed() // the outcome line says it; no stage header or status line
	var b strings.Builder
	if r.tty {
		if r.status {
			b.WriteString(eraseLine)
		}
	} else if p.Stage != r.stage && !final {
		fmt.Fprintf(&b, "[%s]\n", label)
	}
	r.stage = p.Stage
	for _, l := range fresh {
		b.WriteString(printable(l))
		b.WriteByte('\n')
	}
	r.status = false
	if r.tty && !final {
		fmt.Fprintf(&b, "[%s] %s", label, fmtElapsed(p.ElapsedSeconds))
		switch {
		case p.Stage == "push" && p.TotalBytes > 0: // upload progress
			fmt.Fprintf(&b, " %s / %s (%d%%)", fmtSize(p.PushedBytes), fmtSize(p.TotalBytes), p.PushedBytes*100/p.TotalBytes)
		case p.SizeBytes > 0:
			b.WriteString(" " + fmtSize(p.SizeBytes))
		}
		r.status = true
	}
	_, _ = io.WriteString(r.e.Out, b.String())
}

func runSnapshots(e *Env, a *parsed) int {
	if len(a.pos) > 0 {
		return usage(e, find("snapshots"), "snapshots takes no arguments; use `snapshot show <id>`")
	}
	dc, dname := splitRef(a.val["devpod"]) // --devpod <cluster>/<name> says the cluster too
	path := scoped(e, dc, "/api/snapshots")
	if a.has["all"] {
		path += "?scope=all"
	}
	if d := dname; d != "" {
		sep := "?"
		if a.has["all"] {
			sep = "&"
		}
		path += sep + "devpod=" + url.QueryEscape(d)
	}
	raw, err := call(e, "GET", path, nil, nil)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	// {"items": […]} like the other lists; a bare array is accepted too
	var list struct {
		Items []snapView `json:"items"`
	}
	if json.Unmarshal(raw, &list) != nil {
		if err := json.Unmarshal(raw, &list.Items); err != nil {
			return fail(e, fmt.Errorf("unexpected API answer: %w", err))
		}
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(e.Out, "no snapshots; make one with `snapshot <devpod>`")
		return ExitOK
	}
	table(e, "ID\tDEVPOD\tIMAGE\tSIZE\tSTATUS\tAGE", func(w io.Writer) {
		for i := range list.Items {
			v := &list.Items[i]
			created := time.Time{}
			if v.CreatedAt != nil {
				created = *v.CreatedAt
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", joinRef(v.Cluster, v.ID), dash(v.DevPod), dash(v.Image), fmtSize(v.SizeBytes), dash(v.Phase), age(e.Now(), created))
		}
	})
	return ExitOK
}

// shanghai: tags are stamped in the deployment's local time.
var shanghai = time.FixedZone("Asia/Shanghai", 8*3600)

// withTag replaces (or adds) the tag of an image reference; the port of a
// registry host is not a tag.
func withTag(image, tag string) string {
	slash := strings.LastIndexByte(image, '/')
	if i := strings.IndexByte(image[slash+1:], ':'); i >= 0 {
		image = image[:slash+1+i]
	}
	return image + ":" + tag
}
