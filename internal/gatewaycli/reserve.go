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

// Node reservations: a period on a node held for one user (the console's
// 节点预约). Reservations are a cluster's, like its nodes.

const reserveLong = `reserve [list]             reservations from now on (--days N ahead, default 14)
reserve rules              the steps, the longest period and how far ahead one may reserve
reserve add <node> --start T (--end T | --for D) [--dry-run]
                           hold a node for yourself; --dry-run says how many DevPods
                           of others it would stop, and holds nothing
reserve change <id> [--start T] [--end T]
reserve cancel <id>

Times: 2026-11-14T09:00 or 09:00 (today) in the console's time zone (reserve
rules), an RFC 3339 time, now, or +2h / +30m from now. --for 4h instead of
--end. Periods are in whole steps of the rules' granularity.`

func reserveCommands() []*command {
	return []*command{
		{name: "reserve", aliases: []string{"reservations"},
			args:  "[list [--days N] | rules | add <node> --start T (--end T | --for D) [--dry-run] | change <id> [--start T] [--end T] | cancel <id>] [--cluster C]",
			brief: "[list|rules|add|change|cancel …]",
			short: "node reservations: list, the rules, reserve a node, change or cancel one",
			long:  reserveLong,
			vals:  []string{"start", "end", "for", "days"}, bools: []string{"dry-run"}, run: runReserve},
	}
}

type reservation struct {
	Cluster     string    `json:"cluster"`
	ID          string    `json:"id"`
	Node        string    `json:"node"`
	User        string    `json:"user"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Phase       string    `json:"phase"`
	Mine        bool      `json:"mine"`
	CancelledBy string    `json:"cancelledBy"`
}

type reserveRules struct {
	Granularity int64     `json:"granularitySeconds"`
	MaxDuration int64     `json:"maxDurationSeconds"`
	MaxAdvance  int64     `json:"maxAdvanceSeconds"`
	ClearNotice int64     `json:"clearNoticeSeconds"`
	Timezone    string    `json:"timezone"`
	Now         time.Time `json:"now"`
}

func (r *reserveRules) loc() *time.Location {
	if l, err := time.LoadLocation(r.Timezone); err == nil && r.Timezone != "" {
		return l
	}
	return time.Local
}

func rules(e *Env, cluster string) (*reserveRules, []byte, error) {
	var r reserveRules
	raw, err := call(e, "GET", scoped(e, cluster, "/api/reservations/rules"), nil, &r)
	return &r, raw, err
}

func runReserve(e *Env, a *parsed) int {
	c := find("reserve")
	verb, rest := "list", a.pos
	if len(a.pos) > 0 {
		verb, rest = a.pos[0], a.pos[1:]
	}
	switch verb {
	case "list", "ls":
		return reserveList(e, a)
	case "rules":
		r, raw, err := rules(e, "")
		if err != nil {
			return fail(e, err)
		}
		if a.json {
			printJSON(e, raw)
			return ExitOK
		}
		sec := func(n int64) string { return (time.Duration(n) * time.Second).String() }
		fmt.Fprintf(e.Out, "step:          %s\nlongest:       %s\nahead:         up to %s\nnotice:        %s before a reservation starts, others' DevPods on the node are warned\ntime zone:     %s\n",
			sec(r.Granularity), sec(r.MaxDuration), sec(r.MaxAdvance), sec(r.ClearNotice), r.loc())
		return ExitOK
	case "add":
		if len(rest) != 1 {
			return usage(e, c, "reserve add takes one node")
		}
		cluster, node := splitRef(rest[0])
		r, _, err := rules(e, cluster)
		if err != nil {
			return fail(e, err)
		}
		start, end, err := period(e, r, a, true)
		if err != nil {
			return usage(e, c, err.Error())
		}
		path := scoped(e, cluster, "/api/reservations")
		if a.has["dry-run"] {
			path += "?dryRun=true"
		}
		body := map[string]any{"node": node, "start": start.UTC().Format(time.RFC3339), "end": end.UTC().Format(time.RFC3339)}
		if a.has["dry-run"] {
			var p struct {
				WillHibernate int `json:"willHibernate"`
			}
			raw, err := call(e, "POST", path, body, &p)
			if err != nil {
				return fail(e, err)
			}
			if a.json {
				printJSON(e, raw)
				return ExitOK
			}
			fmt.Fprintf(e.Out, "%s is free then; %d DevPods of others would be stopped when it starts\n", node, p.WillHibernate)
			return ExitOK
		}
		var v reservation
		raw, err := call(e, "POST", path, body, &v)
		if err != nil {
			return fail(e, err)
		}
		if a.json {
			printJSON(e, raw)
			return ExitOK
		}
		fmt.Fprintf(e.Out, "reserved %s %s – %s (id %s)\n", node, v.Start.In(r.loc()).Format("2006-01-02 15:04"), v.End.In(r.loc()).Format("15:04 MST"), joinRef(v.Cluster, v.ID))
		return ExitOK
	case "change":
		if len(rest) != 1 {
			return usage(e, c, "reserve change takes one reservation id")
		}
		cluster, id := splitRef(rest[0])
		r, _, err := rules(e, cluster)
		if err != nil {
			return fail(e, err)
		}
		body := map[string]any{}
		for _, f := range []string{"start", "end"} {
			if s, ok := a.val[f]; ok {
				t, err := when(s, r.loc(), e.Now())
				if err != nil {
					return usage(e, c, err.Error())
				}
				body[f] = t.UTC().Format(time.RFC3339)
			}
		}
		if len(body) == 0 {
			return usage(e, c, "give --start and/or --end")
		}
		var v reservation
		raw, err := call(e, "PATCH", scoped(e, cluster, "/api/reservations/"+url.PathEscape(id)), body, &v)
		if err != nil {
			return fail(e, err)
		}
		if a.json {
			printJSON(e, raw)
			return ExitOK
		}
		fmt.Fprintf(e.Out, "%s: now %s – %s\n", rest[0], v.Start.In(r.loc()).Format("2006-01-02 15:04"), v.End.In(r.loc()).Format("15:04 MST"))
		return ExitOK
	case "cancel", "rm":
		if len(rest) == 0 {
			return usage(e, c, "reserve cancel takes reservation ids")
		}
		a.pos = rest
		return each(e, a, c, "cancelled", func(ref string) (string, error) {
			cluster, id := splitRef(ref)
			_, err := call(e, "DELETE", scoped(e, cluster, "/api/reservations/"+url.PathEscape(id)), nil, nil)
			return "", err
		})
	}
	return usage(e, c, fmt.Sprintf("unknown reserve %q: list, rules, add, change or cancel", verb))
}

func reserveList(e *Env, a *parsed) int {
	days := 14
	if s, ok := a.val["days"]; ok {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return usage(e, find("reserve"), "--days takes a number of days")
		}
		days = n
	}
	now := e.Now().UTC()
	q := url.Values{"from": {now.Add(-24 * time.Hour).Format(time.RFC3339)}, "to": {now.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)}}
	var list struct {
		Items []reservation `json:"items"`
	}
	raw, err := call(e, "GET", scoped(e, "", "/api/reservations")+"?"+q.Encode(), nil, &list)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	var items []reservation
	for _, r := range list.Items {
		if r.End.After(now) && r.Phase != "Cancelled" {
			items = append(items, r)
		}
	}
	if len(items) == 0 {
		fmt.Fprintln(e.Out, "no reservations ahead; reserve a node with `reserve add <node> --start T --for D`")
		return ExitOK
	}
	r, _, _ := rules(e, "")
	loc := time.Local
	if r != nil {
		loc = r.loc()
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Start.Before(items[j].Start) })
	table(e, "ID\tNODE\tUSER\tSTART\tEND\tPHASE", func(w io.Writer) {
		for _, v := range items {
			user := v.User
			if v.Mine {
				user += " (you)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", joinRef(v.Cluster, v.ID), joinRef(v.Cluster, v.Node), user,
				v.Start.In(loc).Format("01-02 15:04"), v.End.In(loc).Format("01-02 15:04"), v.Phase)
		}
	})
	return ExitOK
}

// period reads --start and --end (or --for).
func period(e *Env, r *reserveRules, a *parsed, need bool) (start, end time.Time, err error) {
	s, ok := a.val["start"]
	if !ok && need {
		return start, end, errors.New("give --start")
	}
	if start, err = when(s, r.loc(), e.Now()); err != nil {
		return
	}
	switch {
	case a.val["end"] != "" && a.val["for"] != "":
		return start, end, errors.New("give --end or --for, not both")
	case a.val["for"] != "":
		d, derr := time.ParseDuration(a.val["for"])
		if derr != nil || d <= 0 {
			return start, end, fmt.Errorf("--for takes a duration like 4h or 90m, not %q", a.val["for"])
		}
		end = start.Add(d)
	case a.val["end"] != "":
		if end, err = when(a.val["end"], r.loc(), e.Now()); err != nil {
			return
		}
	default:
		return start, end, errors.New("give --end or --for")
	}
	if !end.After(start) {
		return start, end, errors.New("the end must be after the start")
	}
	return start, end, nil
}

// when reads a time: RFC 3339, 2006-01-02T15:04 or 15:04 in loc, now, +2h.
func when(s string, loc *time.Location, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "now":
		return now, nil
	case strings.HasPrefix(s, "+"):
		d, err := time.ParseDuration(s[1:])
		if err != nil {
			return time.Time{}, fmt.Errorf("%q: +2h, +30m … from now", s)
		}
		return now.Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, f := range []string{"2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(f, s, loc); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("15:04", s, loc); err == nil {
		n := now.In(loc)
		return time.Date(n.Year(), n.Month(), n.Day(), t.Hour(), t.Minute(), 0, 0, loc), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a time: 2026-11-14T09:00, 09:00, now, +2h or RFC 3339", s)
}
