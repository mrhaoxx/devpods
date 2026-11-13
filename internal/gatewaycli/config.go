// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package gatewaycli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Saved configs (config center spec §5.2): DevPod YAML with variables,
// kept per user, shared or public, and launched as one or many DevPods.

// maxConfigContent mirrors the console's limit on a config's text.
const maxConfigContent = 256 << 10

const configLong = `A config is a DevPod manifest saved as text, with variables: ${NAME} (the
DevPod's name — you give the part after your user name), ${NODE}, ${OWNER},
${INDEX} (from 0), ${COUNT}, ${LEADER} (the name of number 0), and any ${VAR}
or ${VAR:-default} of your own.

  config [list] [--mine|--shared|--public]   the configs you may use
  config show <name>                         its details and YAML
  config save <id> [--title T] [--description D] [--public]
                                             save the YAML on standard input as <you>-<id>;
                                             an existing one gets the new YAML (and the flags given)
  config share <name> [user…]                who may use it besides you (no user: nobody)
  config public <name> on|off                open it to everyone signed in, or not
  config delete <name>
  config launch <name> [--set VAR=value]… [--node N]… [--count N] [--dry-run] [--cluster C]
                                             create DevPods from it (with several clusters:
                                             in --cluster C)

<name> is the config's full name as the list shows it; your own may be given
without your user name. launch creates one DevPod per --node for a config with
${NODE}, otherwise --count of them (default 1); several get -0, -1, … after the
name and form a group. Everything is checked first: if one cannot be created, none is. --dry-run
only checks, and prints the YAML each DevPod would be created from.`

func configCommands() []*command {
	return []*command{
		{group: "Configs", name: "config", aliases: []string{"configs"}, brief: "[list|show|save|share|public|delete|launch …]",
			args:  "[list | show <name> | save <id> | share <name> [user…] | public <name> on|off | delete <name> | launch <name> [--set VAR=value]… [--node N]… [--count N] [--dry-run]]",
			short: "saved DevPod configs: list, save, share, and create DevPods from one",
			long:  configLong,
			vals:  []string{"title", "description", "set", "node", "count"},
			bools: []string{"mine", "shared", "public", "dry-run"},
			run:   runConfig},
	}
}

type configVar struct {
	Name     string `json:"name"`
	Builtin  bool   `json:"builtin"`
	Required bool   `json:"required"`
	Default  string `json:"default"`
}

type configView struct {
	Name        string      `json:"name"`
	Owner       string      `json:"owner"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Content     string      `json:"content"`
	SharedWith  []string    `json:"sharedWith"`
	Public      bool        `json:"public"`
	Variables   []configVar `json:"variables"`
	Mine        bool        `json:"mine"`
	UpdatedAt   string      `json:"updatedAt"`
}

// access says who may use the config, in a word.
func (v *configView) access() string {
	switch {
	case v.Public:
		return "public"
	case !v.Mine:
		return "shared"
	case len(v.SharedWith) > 0:
		return "shared:" + strconv.Itoa(len(v.SharedWith))
	}
	return "private"
}

func configPath(name string) string { return "/api/configs/" + url.PathEscape(name) }

func runConfig(e *Env, a *parsed) int {
	c := find("config")
	sub := "list"
	if len(a.pos) > 0 {
		sub = a.pos[0]
	}
	args := a.pos
	if len(args) > 0 {
		args = args[1:]
	}
	switch sub {
	case "list", "ls":
		return configList(e, c, a)
	case "show", "get":
		return configShow(e, c, a, args)
	case "save":
		return configSave(e, c, a, args)
	case "share":
		return configShare(e, c, a, args)
	case "public":
		return configPublic(e, c, a, args)
	case "delete", "rm":
		return configDelete(e, c, a, args)
	case "launch":
		return configLaunch(e, c, a, args)
	}
	return usage(e, c, fmt.Sprintf("unknown subcommand %q", sub))
}

func configList(e *Env, c *command, a *parsed) int {
	scope := ""
	for _, s := range []string{"mine", "shared", "public"} {
		if a.has[s] {
			if scope != "" {
				return usage(e, c, "give one of --mine, --shared, --public")
			}
			scope = s
		}
	}
	path := "/api/configs"
	if scope != "" {
		path += "?scope=" + scope
	}
	var list struct {
		Items []configView `json:"items"`
	}
	raw, err := call(e, http.MethodGet, path, nil, &list)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(e.Out, "no configs")
		return ExitOK
	}
	table(e, "NAME\tOWNER\tACCESS\tVARIABLES\tTITLE", func(w io.Writer) {
		for i := range list.Items {
			v := &list.Items[i]
			names := make([]string, len(v.Variables))
			for j, x := range v.Variables {
				names[j] = x.Name
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", v.Name, v.Owner, v.access(), dash(strings.Join(names, ",")), v.Title)
		}
	})
	return ExitOK
}

// resolveConfig finds the config a name on the command line means: the
// caller's own <user>-<name> first, then the name as given.
func resolveConfig(e *Env, name string) (*configView, []byte, error) {
	var first error
	for _, cand := range []string{e.User + "-" + name, name} {
		var v configView
		raw, err := call(e, http.MethodGet, configPath(cand), nil, &v)
		if err == nil {
			return &v, raw, nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			return nil, nil, err
		}
		if first == nil {
			first = err
		}
	}
	return nil, nil, first
}

func configShow(e *Env, c *command, a *parsed, args []string) int {
	if len(args) != 1 {
		return usage(e, c, "show takes one config name")
	}
	v, raw, err := resolveConfig(e, args[0])
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	fmt.Fprintf(e.Out, "name:        %s\ntitle:       %s\nowner:       %s\naccess:      %s\n", v.Name, v.Title, v.Owner, v.access())
	if v.Description != "" {
		fmt.Fprintf(e.Out, "description: %s\n", v.Description)
	}
	if len(v.SharedWith) > 0 {
		fmt.Fprintf(e.Out, "shared with: %s\n", strings.Join(v.SharedWith, ", "))
	}
	vars := make([]string, len(v.Variables))
	for i, x := range v.Variables {
		vars[i] = x.Name
		if !x.Required && !x.Builtin {
			vars[i] += fmt.Sprintf(" (default %s)", x.Default)
		}
	}
	fmt.Fprintf(e.Out, "variables:   %s\n", dash(strings.Join(vars, ", ")))
	if v.UpdatedAt != "" {
		fmt.Fprintf(e.Out, "updated:     %s\n", v.UpdatedAt)
	}
	fmt.Fprintf(e.Out, "---\n%s", v.Content)
	if !strings.HasSuffix(v.Content, "\n") {
		fmt.Fprintln(e.Out)
	}
	return ExitOK
}

// readStdin reads standard input to its end, line by line.
func readStdin(e *Env) (string, error) {
	if e.StdinLine == nil {
		return "", nil
	}
	var b strings.Builder
	for {
		line, err := e.StdinLine()
		if err != nil {
			if errors.Is(err, errInterrupted) {
				return "", err
			}
			return b.String(), nil
		}
		b.WriteString(line)
		b.WriteByte('\n')
		if b.Len() > maxConfigContent {
			return "", fmt.Errorf("the YAML is larger than a config may be (%d KiB)", maxConfigContent>>10)
		}
	}
}

func configSave(e *Env, c *command, a *parsed, args []string) int {
	if len(args) != 1 {
		return usage(e, c, "save takes one id: the config becomes <you>-<id>")
	}
	id := args[0]
	content, err := readStdin(e)
	if err != nil {
		return usage(e, c, err.Error())
	}
	if strings.TrimSpace(content) == "" {
		return usage(e, c, "give the YAML on standard input: ssh … config save "+id+" < devpod.yaml")
	}
	_, hasTitle := a.val["title"]
	_, hasDesc := a.val["description"]
	body := map[string]any{"id": id, "title": id, "content": content}
	if hasTitle {
		body["title"] = a.val["title"]
	}
	if hasDesc {
		body["description"] = a.val["description"]
	}
	if a.has["public"] {
		body["public"] = true
	}
	var v configView
	verb := "saved"
	raw, err := call(e, http.MethodPost, "/api/configs", body, &v)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "CONFIG_EXISTS" {
		// the caller's own config of that id: the new YAML, and only
		// what was said on the command line besides
		patch := map[string]any{"content": content}
		if hasTitle {
			patch["title"] = a.val["title"]
		}
		if hasDesc {
			patch["description"] = a.val["description"]
		}
		if a.has["public"] {
			patch["public"] = true
		}
		verb = "updated"
		raw, err = call(e, http.MethodPatch, configPath(e.User+"-"+id), patch, &v)
	}
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	fmt.Fprintf(e.Out, "%s %s (%d variables)\n", verb, v.Name, len(v.Variables))
	return ExitOK
}

// configPatch changes one config found by name and reports with done.
func configPatch(e *Env, a *parsed, name string, patch map[string]any, done func(v *configView) string) int {
	have, _, err := resolveConfig(e, name)
	if err != nil {
		return fail(e, err)
	}
	var v configView
	raw, err := call(e, http.MethodPatch, configPath(have.Name), patch, &v)
	if err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, raw)
		return ExitOK
	}
	fmt.Fprintln(e.Out, done(have))
	return ExitOK
}

func configShare(e *Env, c *command, a *parsed, args []string) int {
	if len(args) == 0 {
		return usage(e, c, "share takes a config name, then the users (none: share with nobody)")
	}
	users := append([]string{}, args[1:]...)
	return configPatch(e, a, args[0], map[string]any{"sharedWith": users}, func(v *configView) string {
		if len(users) == 0 {
			return v.Name + " is shared with nobody"
		}
		return v.Name + " is shared with " + strings.Join(users, ", ")
	})
}

func configPublic(e *Env, c *command, a *parsed, args []string) int {
	if len(args) != 2 || (args[1] != "on" && args[1] != "off") {
		return usage(e, c, "public takes a config name and on or off")
	}
	on := args[1] == "on"
	return configPatch(e, a, args[0], map[string]any{"public": on}, func(v *configView) string {
		if on {
			return v.Name + " is public: everyone signed in may use it"
		}
		return v.Name + " is no longer public"
	})
}

func configDelete(e *Env, c *command, a *parsed, args []string) int {
	if len(args) != 1 {
		return usage(e, c, "delete takes one config name")
	}
	have, _, err := resolveConfig(e, args[0])
	if err != nil {
		return fail(e, err)
	}
	if _, err := call(e, http.MethodDelete, configPath(have.Name), nil, nil); err != nil {
		return fail(e, err)
	}
	if a.json {
		printJSON(e, []byte(fmt.Sprintf(`{"name":%q,"deleted":true}`, have.Name)))
		return ExitOK
	}
	fmt.Fprintf(e.Out, "deleted %s\n", have.Name)
	return ExitOK
}

type launchItem struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Node    string `json:"node"`
	OK      bool   `json:"ok"`
	Code    string `json:"code"`
	Message string `json:"message"`
	YAML    string `json:"yaml"`
}

func configLaunch(e *Env, c *command, a *parsed, args []string) int {
	if len(args) != 1 {
		return usage(e, c, "launch takes one config name")
	}
	body := map[string]any{}
	if sets := a.all["set"]; len(sets) > 0 {
		vars := map[string]string{}
		for _, s := range sets {
			k, v, ok := strings.Cut(s, "=")
			if !ok || k == "" {
				return usage(e, c, fmt.Sprintf("--set %s: write VAR=value", s))
			}
			if _, twice := vars[k]; twice {
				return usage(e, c, fmt.Sprintf("--set %s is given twice", k))
			}
			vars[k] = v
		}
		body["vars"] = vars
	}
	nodes := a.all["node"]
	if len(nodes) > 0 {
		body["nodes"] = nodes
	}
	if s, given := a.val["count"]; given {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return usage(e, c, "--count takes a number, 1 or more")
		}
		if len(nodes) > 0 {
			return usage(e, c, "--node already says how many: one DevPod per node; --count is for configs without ${NODE}")
		}
		body["count"] = n
	}
	dry := a.has["dry-run"]
	if dry {
		body["dryRun"] = true
	}
	have, _, err := resolveConfig(e, args[0])
	if err != nil {
		return fail(e, err)
	}

	// 200: all created (or, dry, all would be). 207: some were not.
	// 422: the check refused the launch and nothing was created — its
	// body lists every DevPod like the others do.
	rejected := false
	raw, err := call(e, http.MethodPost, scoped(e, "", configPath(have.Name)+"/launch"), body, nil)
	if err != nil {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity || len(apiErr.Body) == 0 {
			return fail(e, err)
		}
		rejected, raw = true, apiErr.Body
		fmt.Fprintf(e.Err, "error: %s\n", err)
	}
	var res struct {
		Group string       `json:"group"`
		Items []launchItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fail(e, fmt.Errorf("unexpected API answer: %w", err))
	}
	code := ExitOK
	if rejected {
		code = ExitAPI
	}
	for _, it := range res.Items {
		if !it.OK {
			code = ExitAPI
		}
	}
	if a.json {
		printJSON(e, raw)
		return code
	}
	if dry && !rejected {
		for _, it := range res.Items {
			fmt.Fprintf(e.Out, "--- %s\n%s", it.Name, it.YAML)
			if !strings.HasSuffix(it.YAML, "\n") {
				fmt.Fprintln(e.Out)
			}
		}
		return code
	}
	for _, it := range res.Items {
		switch {
		case !it.OK:
			fmt.Fprintf(e.Out, "FAIL %s  %s  %s: %s\n", dash(it.Name), dash(it.Node), it.Code, it.Message)
		case rejected:
			// it passed the check, but nothing of a rejected launch exists
			fmt.Fprintf(e.Out, "pass %s  %s\n", it.Name, dash(it.Node))
		default:
			fmt.Fprintf(e.Out, "ok   %s  %s\n", it.Name, dash(it.Node))
		}
	}
	if res.Group != "" && !rejected {
		fmt.Fprintf(e.Out, "group: %s\n", res.Group)
	}
	return code
}
