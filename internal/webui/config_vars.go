// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/configvars"
)

// What a config says of its variables besides the text: a description
// and a type for each (config-centric create spec §2.1), kept in an
// annotation of the config. Defaults stay in the text (${X:-default}).

const (
	annOfficial  = "devpod.io/official"
	annVariables = "devpod.io/variables"
)

// The types of a variable: they choose the launch form's control and
// how a value is checked before it is put into the text.
const (
	VarString = "string"
	VarInt    = "int"
	VarChoice = "choice"
	VarNode   = "node"
	VarImage  = "image"
)

const (
	maxDecls       = 64
	maxDeclOptions = 64
	maxDeclDesc    = 200 // characters
)

var varNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// VarDecl is what a config says of one of its variables.
type VarDecl struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// ConfigVarView is a variable as the API shows it: what the text says
// (default, required) and what the config says of it.
type ConfigVarView struct {
	configvars.Var
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Options     []string `json:"options,omitempty"`
}

func knownType(t string) bool {
	switch t {
	case VarString, VarInt, VarChoice, VarNode, VarImage:
		return true
	}
	return false
}

// cleanDecls checks declarations against the text they describe and
// puts them in order: the text's order, one per variable, none for a
// variable the text does not have. The type "string" is not written.
func cleanDecls(content string, in []VarDecl) ([]VarDecl, *serverErr) {
	if len(in) > maxDecls {
		return nil, badRequest("at most %d variables are described", maxDecls)
	}
	byName := map[string]VarDecl{}
	for _, d := range in {
		if !varNameRE.MatchString(d.Name) {
			return nil, badRequest("%q is not a variable name (A-Z, 0-9 and _, starting with a letter)", d.Name)
		}
		if configvars.ServerFilled(d.Name) || d.Name == configvars.Name || d.Name == configvars.Node {
			return nil, badRequest("%s is a built-in variable: it cannot be described", d.Name)
		}
		if d.Type == "" {
			d.Type = VarString
		}
		if !knownType(d.Type) {
			return nil, badRequest("variable %s: type %q is not one of string, int, choice, node, image", d.Name, d.Type)
		}
		if utf8.RuneCountInString(d.Description) > maxDeclDesc {
			return nil, badRequest("variable %s: the description is at most %d characters", d.Name, maxDeclDesc)
		}
		if d.Type == VarChoice {
			if len(d.Options) == 0 || len(d.Options) > maxDeclOptions {
				return nil, badRequest("variable %s: a choice has 1 to %d options", d.Name, maxDeclOptions)
			}
			for _, o := range d.Options {
				if o == "" || len(o) > configvars.MaxValueLen || !printable(o) {
					return nil, badRequest("variable %s: an option is empty, too long or has a line break", d.Name)
				}
			}
		} else {
			d.Options = nil
		}
		if _, twice := byName[d.Name]; !twice {
			byName[d.Name] = d
		}
	}
	out := []VarDecl{}
	for _, v := range configvars.Parse(content) {
		d, ok := byName[v.Name]
		if !ok {
			continue
		}
		if d.Type == VarString {
			d.Type = ""
		}
		out = append(out, d)
	}
	return out, nil
}

func printable(s string) bool {
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// readDecls reads a config's declarations; a broken annotation is none.
func readDecls(c *devpodv1alpha1.DevPodConfig) []VarDecl {
	raw := c.Annotations[annVariables]
	if raw == "" {
		return nil
	}
	var out []VarDecl
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// writeDecls stores declarations on the config (none: no annotation).
func writeDecls(c *devpodv1alpha1.DevPodConfig, decls []VarDecl) {
	if len(decls) == 0 {
		delete(c.Annotations, annVariables)
		return
	}
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	b, err := json.Marshal(decls)
	if err != nil {
		panic(fmt.Sprintf("marshal declarations: %v", err)) // plain strings: cannot fail
	}
	c.Annotations[annVariables] = string(b)
}

// declOf is the declaration of name, or a plain string one.
func declOf(decls []VarDecl, name string) VarDecl {
	for _, d := range decls {
		if d.Name == name {
			if d.Type == "" {
				d.Type = VarString
			}
			return d
		}
	}
	return VarDecl{Name: name, Type: VarString}
}

// varViews: the text's variables with what the config says of them.
func varViews(c *devpodv1alpha1.DevPodConfig) []ConfigVarView {
	decls := readDecls(c)
	out := []ConfigVarView{}
	for _, v := range configvars.Parse(c.Spec.Content) {
		d := declOf(decls, v.Name)
		out = append(out, ConfigVarView{Var: v, Description: d.Description, Type: d.Type, Options: d.Options})
	}
	return out
}

func isOfficial(c *devpodv1alpha1.DevPodConfig) bool {
	return c.Annotations[annOfficial] == "true"
}
