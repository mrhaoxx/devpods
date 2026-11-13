// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Registry credentials (snapshot v2 spec §5.3): the caller's own logins
// for image registries. They pull private images for the caller's
// DevPods and push when a snapshot goes to that registry.

const registryLong = `registry list shows the registries you saved a login for.

registry add <host> saves (or replaces) the login for one registry, e.g.
"registry add harbor.example.com --username bob". The password is asked
for without echo on a terminal, or read from the first line of standard
input (--password-stdin forces that; it needs --username). The password
never goes on the command line and is never shown again.

registry rm <host> removes it.

A saved login is used to pull private images for the DevPods you start
from then on, and to push when "snapshot --image" names that registry.`

func registryCommands() []*command {
	return []*command{
		{name: "registry", aliases: []string{"registries"}, args: "[list] | add <host> [--username U] [--password-stdin] | rm <host>",
			brief: "[list|add|rm …]",
			short: "your saved registry logins: pull private images, push snapshots",
			long:  registryLong,
			vals:  []string{"username"}, bools: []string{"password-stdin"}, run: runRegistry},
	}
}

type registryView struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Pending  bool   `json:"pending"`
}

func registryPath(host string) string { return "/api/me/registries/" + url.PathEscape(host) }

// registryHost tidies what was typed as a registry: scheme and path
// dropped, lower-cased. The API validates the rest.
func registryHost(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// imageRegistry is the registry an image reference points at: the part
// before the first "/" when it has a "." or a ":" or is "localhost";
// anything else lives on Docker Hub.
func imageRegistry(ref string) string {
	first, _, ok := strings.Cut(ref, "/")
	first = strings.ToLower(first)
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return "docker.io"
}

func runRegistry(e *Env, a *parsed) int {
	c := find("registry")
	verb, rest := "list", a.pos
	if len(a.pos) > 0 {
		verb, rest = a.pos[0], a.pos[1:]
	}
	_, hasUser := a.val["username"]
	if verb != "add" && (hasUser || a.has["password-stdin"]) {
		return usage(e, c, "--username and --password-stdin only apply to `registry add`")
	}
	switch verb {
	case "list", "ls":
		if len(rest) != 0 {
			return usage(e, c, "registry list takes no arguments")
		}
		return registryList(e, a)
	case "add":
		if len(rest) != 1 || registryHost(rest[0]) == "" {
			return usage(e, c, "registry add takes exactly one registry host")
		}
		return registryAdd(e, a, c, registryHost(rest[0]))
	case "rm", "remove", "delete":
		if len(rest) != 1 || registryHost(rest[0]) == "" {
			return usage(e, c, "registry rm takes exactly one registry host")
		}
		return registryRemove(e, a, registryHost(rest[0]))
	}
	return usage(e, c, fmt.Sprintf("unknown registry sub-command %q; use list, add or rm", verb))
}

func registryList(e *Env, a *parsed) int {
	raw, err := call(e, "GET", "/api/me/registries", nil, nil)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	var list struct {
		Items []registryView `json:"items"`
	}
	if json.Unmarshal(raw, &list) != nil {
		if err := json.Unmarshal(raw, &list.Items); err != nil {
			return fail(e, fmt.Errorf("unexpected API answer: %w", err))
		}
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(e.Out, "no saved registry logins; add one with `registry add <host>`")
		return ExitOK
	}
	table(e, "HOST\tUSERNAME\tSTATUS", func(w io.Writer) {
		for _, r := range list.Items {
			status := "ready"
			if r.Pending {
				status = "pending" // not written to the cluster yet
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", r.Host, dash(r.Username), status)
		}
	})
	return ExitOK
}

func registryAdd(e *Env, a *parsed, c *command, host string) int {
	auth, err := credentials(e, a)
	if err != nil {
		if errors.Is(err, errInterrupted) {
			return ExitInterrupted
		}
		return usage(e, c, err.Error())
	}
	raw, err := call(e, "PUT", registryPath(host), auth, nil)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		if len(strings.TrimSpace(string(raw))) > 0 {
			printJSON(e, raw)
		} else { // the API answered without a body
			b, _ := json.Marshal(map[string]any{"host": host, "username": auth.Username, "saved": true})
			printJSON(e, b)
		}
		return ExitOK
	}
	fmt.Fprintf(e.Out, "saved the login for %s (user %s)\nit pulls private images for DevPods you start from now on, and pushes when `snapshot --image` names %s\n", host, auth.Username, host)
	return ExitOK
}

func registryRemove(e *Env, a *parsed, host string) int {
	raw, err := call(e, "DELETE", registryPath(host), nil, nil)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		if len(strings.TrimSpace(string(raw))) > 0 {
			printJSON(e, raw)
		} else {
			b, _ := json.Marshal(map[string]any{"host": host, "removed": true})
			printJSON(e, b)
		}
		return ExitOK
	}
	fmt.Fprintf(e.Out, "removed the login for %s; running DevPods are not affected\n", host)
	return ExitOK
}
