// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// events, yaml, rebuild: what the console's DevPod page does beside the
// basics.

func runEvents(e *Env, a *parsed) int {
	if len(a.pos) != 1 {
		return usage(e, find("events"), "events takes exactly one name")
	}
	var list struct {
		Items []struct {
			Type           string    `json:"type"`
			Reason         string    `json:"reason"`
			Message        string    `json:"message"`
			Count          int       `json:"count"`
			LastTimestamp  time.Time `json:"lastTimestamp"`
			EventTime      time.Time `json:"eventTime"`
			InvolvedObject struct {
				Kind string `json:"kind"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	raw, err := call(e, "GET", dpPath(e, a.pos[0])+"/events", nil, &list)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(e.Out, "no recent events")
		return ExitOK
	}
	at := func(i int) time.Time {
		if t := list.Items[i].LastTimestamp; !t.IsZero() {
			return t
		}
		return list.Items[i].EventTime
	}
	sort.SliceStable(list.Items, func(i, j int) bool { return at(i).Before(at(j)) })
	table(e, "AGE\tTYPE\tREASON\tOBJECT\tMESSAGE", func(w io.Writer) {
		for i, ev := range list.Items {
			reason := ev.Reason
			if ev.Count > 1 {
				reason += fmt.Sprintf(" (×%d)", ev.Count)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", age(e.Now(), at(i)), dash(ev.Type), dash(reason), dash(ev.InvolvedObject.Kind),
				strings.ReplaceAll(ev.Message, "\n", " "))
		}
	})
	return ExitOK
}

func runYAML(e *Env, a *parsed) int {
	c := find("yaml")
	if len(a.pos) != 1 {
		return usage(e, c, "yaml takes exactly one name")
	}
	path := dpPath(e, a.pos[0]) + "/yaml"
	if !a.has["apply"] {
		var y struct {
			YAML string `json:"yaml"`
		}
		raw, err := call(e, "GET", path, nil, &y)
		if err != nil {
			return fail(e, err)
		}
		if a.json {
			printJSON(e, raw)
			return ExitOK
		}
		fmt.Fprint(e.Out, strings.TrimRight(y.YAML, "\n")+"\n")
		return ExitOK
	}
	text, err := readAll(e)
	if err != nil {
		return usage(e, c, err.Error())
	}
	var r patchResult
	raw, err := call(e, "PUT", path, map[string]any{"yaml": text}, &r)
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

// readAll reads standard input to its end: a YAML, a key.
func readAll(e *Env) (string, error) {
	if e.StdinLine == nil {
		return "", errors.New("give it on standard input: ssh <you>+devpod@<gateway> … < file")
	}
	var b strings.Builder
	for {
		line, err := e.StdinLine()
		if line != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
		if err != nil {
			break
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", errors.New("standard input is empty")
	}
	return b.String(), nil
}

func runRebuild(e *Env, a *parsed) int {
	dry := a.has["dry-run"]
	if dry && !a.json {
		if len(a.pos) == 0 {
			return usage(e, find("rebuild"), "rebuild needs at least one name")
		}
		status := ExitOK
		for _, n := range a.pos {
			var r struct {
				YAML    string `json:"yaml"`
				Rebuild bool   `json:"rebuild"`
			}
			if _, err := call(e, "POST", dpPath(e, n)+"/rebuild-from-config", map[string]any{"dryRun": true}, &r); err != nil {
				fmt.Fprintf(e.Err, "error: %s: %s\n", n, err)
				status = ExitAPI
				continue
			}
			what := "nothing to rebuild: its Pod would not change"
			if r.Rebuild {
				what = "its Pod would be recreated"
			}
			fmt.Fprintf(e.Out, "# %s: %s\n%s\n", n, what, strings.TrimRight(r.YAML, "\n"))
		}
		return status
	}
	body := map[string]any{}
	if dry {
		body["dryRun"] = true
	}
	return each(e, a, find("rebuild"), "rebuilt from its config", func(n string) (string, error) {
		var r patchResult
		_, err := call(e, "POST", dpPath(e, n)+"/rebuild-from-config", body, &r)
		return rebuildNote(r), err
	})
}
