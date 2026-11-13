// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// API tokens and SSH keys: what the console's settings page keeps for you.

const tokenLong = `token [list]               your API tokens
token scopes               what a token may be allowed
token create <name> --scopes a,b [--devpods x,y] [--clusters c,d] [--days N]
                           a token for scripts (Authorization: Bearer …); it is
                           shown once. --devpods / --clusters limit it to those;
                           --days: it expires after that many (default 30)
token rm <id>…`

const keyLong = `key [list]                 your SSH public keys, numbered, with fingerprints
key add [<key>]            add a key given as an argument, or each on standard input:
                             ssh <you>+devpod@<gateway> key add < ~/.ssh/id_ed25519.pub
key rm <number|fingerprint|comment>…`

func accountCommands() []*command {
	return []*command{
		{name: "token", aliases: []string{"tokens"}, args: "[list | scopes | create <name> --scopes a,b [--devpods x,y] [--clusters c,d] [--days N] | rm <id>…]", brief: "[list|scopes|create|rm …]",
			short: "API tokens for scripts: list, create, remove", long: tokenLong,
			vals: []string{"scopes", "devpods", "clusters", "days"}, run: runToken},
		{name: "key", aliases: []string{"keys"}, args: "[list | add [<key>] | rm <number|fingerprint|comment>…]", brief: "[list|add|rm …]",
			short: "your SSH public keys: list, add, remove", long: keyLong, run: runKey},
	}
}

type tokenView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Scopes    []string  `json:"scopes"`
	DevPods   []string  `json:"devpods"`
	Clusters  []string  `json:"clusters"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func runToken(e *Env, a *parsed) int {
	c := find("token")
	verb, rest := "list", a.pos
	if len(a.pos) > 0 {
		verb, rest = a.pos[0], a.pos[1:]
	}
	switch verb {
	case "list", "ls", "scopes":
		var list struct {
			Items  []tokenView `json:"items"`
			Scopes []struct {
				ID    string `json:"id"`
				Group string `json:"group"`
				Label string `json:"label"`
				Admin bool   `json:"admin"`
			} `json:"scopes"`
		}
		raw, err := call(e, "GET", "/api/me/tokens", nil, &list)
		if err != nil {
			return fail(e, err)
		}
		if a.json {
			printJSON(e, raw)
			return ExitOK
		}
		if verb == "scopes" {
			table(e, "SCOPE\tGROUP\tWHAT", func(w io.Writer) {
				for _, s := range list.Scopes {
					label := s.Label
					if s.Admin {
						label += " (admin)"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\n", s.ID, s.Group, label)
				}
			})
			return ExitOK
		}
		if len(list.Items) == 0 {
			fmt.Fprintln(e.Out, "no tokens; make one with `token create <name> --scopes …` (`token scopes` lists them)")
			return ExitOK
		}
		table(e, "ID\tNAME\tSCOPES\tLIMITED TO\tEXPIRES", func(w io.Writer) {
			for _, t := range list.Items {
				limited := strings.Join(append(append([]string{}, t.DevPods...), t.Clusters...), ",")
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, strings.Join(t.Scopes, ","), dash(limited), t.ExpiresAt.Format("2006-01-02"))
			}
		})
		return ExitOK
	case "create":
		if len(rest) != 1 {
			return usage(e, c, "token create takes one name")
		}
		scopes := splitList(a.val["scopes"])
		if len(scopes) == 0 {
			return usage(e, c, "give --scopes (`token scopes` lists them)")
		}
		days := 30
		if s, ok := a.val["days"]; ok {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				return usage(e, c, "--days takes a number of days")
			}
			days = n
		}
		body := map[string]any{"name": rest[0], "scopes": scopes, "devpods": splitList(a.val["devpods"]), "expiresInDays": days}
		if cs := splitList(a.val["clusters"]); len(cs) > 0 {
			body["clusters"] = cs
		}
		var r struct {
			Token string    `json:"token"`
			Info  tokenView `json:"info"`
		}
		raw, err := call(e, "POST", "/api/me/tokens", body, &r)
		if err != nil {
			return fail(e, err)
		}
		if a.json {
			printJSON(e, raw)
			return ExitOK
		}
		fmt.Fprintf(e.Out, "%s\n\ntoken %s (id %s), expires %s. It is shown this once: keep it now.\n", r.Token, r.Info.Name, r.Info.ID, r.Info.ExpiresAt.Format("2006-01-02"))
		return ExitOK
	case "rm", "delete":
		if len(rest) == 0 {
			return usage(e, c, "token rm takes token ids")
		}
		a.pos = rest
		return each(e, a, c, "removed", func(id string) (string, error) {
			_, err := call(e, "DELETE", "/api/me/tokens/"+url.PathEscape(id), nil, nil)
			return "", err
		})
	}
	return usage(e, c, fmt.Sprintf("unknown token %q: list, scopes, create or rm", verb))
}

func runKey(e *Env, a *parsed) int {
	c := find("key")
	verb, rest := "list", a.pos
	if len(a.pos) > 0 {
		verb, rest = a.pos[0], a.pos[1:]
	}
	var have struct {
		Pubkeys []string `json:"pubkeys"`
	}
	raw, err := call(e, "GET", "/api/me/pubkeys", nil, &have)
	if err != nil {
		return fail(e, err)
	}
	put := func(keys []string) int {
		if keys == nil {
			keys = []string{}
		}
		raw, err := call(e, "PUT", "/api/me/pubkeys", map[string]any{"pubkeys": keys}, nil)
		if err != nil {
			return fail(e, err)
		}
		if a.json {
			printJSON(e, raw)
		}
		return ExitOK
	}
	switch verb {
	case "list", "ls":
		if a.json {
			printJSON(e, raw)
			return ExitOK
		}
		if len(have.Pubkeys) == 0 {
			fmt.Fprintln(e.Out, "no keys; add one with `key add < ~/.ssh/id_ed25519.pub`")
			return ExitOK
		}
		table(e, "#\tTYPE\tFINGERPRINT\tCOMMENT", func(w io.Writer) {
			for i, k := range have.Pubkeys {
				pk, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(k))
				if err != nil {
					fmt.Fprintf(w, "%d\t?\t(unreadable)\t-\n", i+1)
					continue
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", i+1, pk.Type(), ssh.FingerprintSHA256(pk), dash(comment))
			}
		})
		return ExitOK
	case "add":
		text := strings.Join(rest, " ")
		if text == "" {
			if text, err = readAll(e); err != nil {
				return usage(e, c, err.Error())
			}
		}
		keys := append([]string{}, have.Pubkeys...)
		added := 0
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
			if err != nil {
				return usage(e, c, "not an SSH public key: "+trimTo(line, 40))
			}
			dup := false
			for _, k := range keys {
				if old, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k)); err == nil && string(old.Marshal()) == string(pk.Marshal()) {
					dup = true
				}
			}
			if !dup {
				keys = append(keys, line)
				added++
			}
		}
		if added == 0 {
			fmt.Fprintln(e.Out, "nothing new: you have that key already")
			return ExitOK
		}
		if code := put(keys); code != ExitOK || a.json {
			return code
		}
		fmt.Fprintf(e.Out, "added %d key(s); %d in all\n", added, len(keys))
		return ExitOK
	case "rm", "delete":
		if len(rest) == 0 {
			return usage(e, c, "key rm takes a number, a fingerprint or a comment")
		}
		drop := map[int]bool{}
		for _, which := range rest {
			found := false
			for i, k := range have.Pubkeys {
				pk, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(k))
				if strconv.Itoa(i+1) == which || (err == nil && (ssh.FingerprintSHA256(pk) == which || comment == which)) {
					drop[i], found = true, true
				}
			}
			if !found {
				return usage(e, c, fmt.Sprintf("no key %q: `key list` shows the numbers", which))
			}
		}
		var keep []string
		for i, k := range have.Pubkeys {
			if !drop[i] {
				keep = append(keep, k)
			}
		}
		if code := put(keep); code != ExitOK || a.json {
			return code
		}
		fmt.Fprintf(e.Out, "removed %d key(s); %d left\n", len(drop), len(keep))
		return ExitOK
	}
	return usage(e, c, fmt.Sprintf("unknown key %q: list, add or rm", verb))
}

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
