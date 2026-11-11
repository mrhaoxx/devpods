// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package configvars_test

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mrhaoxx/devpod/internal/configvars"
)

// testdata/cases.json is shared with the console's parser test
// (web/src/features/configs/vars.test.ts).
type testCase struct {
	Name     string            `json:"name"`
	Content  string            `json:"content"`
	Vars     []configvars.Var  `json:"vars"`
	Values   map[string]string `json:"values"`
	Rendered string            `json:"rendered"`
	Error    string            `json:"error"`
}

func describe(err error) string {
	var m *configvars.MissingError
	var u *configvars.UnknownError
	var v *configvars.ValueError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &m):
		return "missing:" + strings.Join(m.Names, ",")
	case errors.As(err, &u):
		return "unknown:" + strings.Join(u.Names, ",")
	case errors.As(err, &v):
		return "value:" + v.Name
	}
	return "other:" + err.Error()
}

func TestCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []testCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := configvars.Parse(tc.Content)
			if len(got) != 0 || len(tc.Vars) != 0 {
				if !reflect.DeepEqual(got, tc.Vars) {
					t.Errorf("Parse = %+v, want %+v", got, tc.Vars)
				}
			}
			for _, v := range tc.Vars {
				if !configvars.Uses(tc.Content, v.Name) {
					t.Errorf("Uses(%s) = false", v.Name)
				}
			}
			if configvars.Uses(tc.Content, "NOT_THERE") {
				t.Error("Uses(NOT_THERE) = true")
			}
			out, err := configvars.Render(tc.Content, tc.Values)
			if d := describe(err); d != tc.Error {
				t.Fatalf("Render error = %q, want %q", d, tc.Error)
			}
			if err == nil && out != tc.Rendered {
				t.Errorf("Render = %q, want %q", out, tc.Rendered)
			}
		})
	}
}

func TestRender_ValueLength(t *testing.T) {
	if _, err := configvars.Render("${X}", map[string]string{"X": strings.Repeat("a", 1024)}); err != nil {
		t.Fatalf("1024 bytes: %v", err)
	}
	_, err := configvars.Render("${X}", map[string]string{"X": strings.Repeat("a", 1025)})
	if describe(err) != "value:X" {
		t.Fatalf("1025 bytes: %v", err)
	}
}

func TestErrorsNameTheVariables(t *testing.T) {
	_, err := configvars.Render("${GPU} ${X}", nil)
	if err == nil || !strings.Contains(err.Error(), "GPU") || !strings.Contains(err.Error(), "X") {
		t.Fatalf("message %v", err)
	}
}
