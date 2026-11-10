// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// maxImportBytes bounds a CSV import body.
const maxImportBytes = 1 << 20

// importRow is one parsed CSV line; Err is set when the line cannot be
// imported whatever the cluster holds.
type importRow struct {
	Line        int
	Username    string
	DisplayName string
	Password    string
	Err         string
}

// parseImportCSV reads `username,displayName[,password]` lines (UI spec
// §5.5): an optional header row, a UTF-8 BOM, CRLF and quoted fields are
// accepted; blank lines are skipped.
func parseImportCSV(body []byte) []importRow {
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(body))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	var rows []importRow
	seen := map[string]int{}
	first := true
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			line := 0
			if errors.As(err, &pe) {
				line = pe.Line
			}
			rows = append(rows, importRow{Line: line, Err: "malformed CSV: " + err.Error()})
			continue
		}
		line, _ := r.FieldPos(0)
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}
		if first {
			first = false
			if strings.EqualFold(rec[0], "username") {
				continue // header
			}
		}
		row := importRow{Line: line, Username: rec[0]}
		if len(rec) > 1 {
			row.DisplayName = rec[1]
		}
		if len(rec) > 2 {
			row.Password = rec[2]
		}
		switch {
		case len(rec) > 3:
			row.Err = "expected username,displayName[,password]"
		case row.Username == "":
			row.Err = "username is empty"
		default:
			if _, err := MapUsername("", row.Username); err != nil {
				row.Err = err.Error()
			} else if prev, dup := seen[row.Username]; dup {
				row.Err = fmt.Sprintf("duplicate username (first on line %d)", prev)
			} else {
				seen[row.Username] = line
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// ImportResult is one CSV line's outcome.
type ImportResult struct {
	Line        int    `json:"line"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName,omitempty"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	// Password is set only for generated passwords, only in the
	// committing response.
	Password string `json:"password,omitempty"`
}

// handleImportUsers serves POST /api/admin/users:import[?dryRun=1].
func (s *Server) handleImportUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if !s.PasswordAuth {
		s.writeErr(w, http.StatusForbidden, "FORBIDDEN", "importing users requires password login to be enabled", nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImportBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.writeErr(w, http.StatusRequestEntityTooLarge, "TOO_LARGE", "the CSV is larger than 1 MiB", nil)
			return
		}
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error(), nil)
		return
	}
	dry := r.URL.Query().Get("dryRun") == "1"
	results := []ImportResult{}
	created := 0
	for _, row := range parseImportCSV(body) {
		res := ImportResult{Line: row.Line, Username: row.Username, DisplayName: row.DisplayName, Error: row.Err}
		if res.Error == "" {
			res.Error, res.Password = s.importUser(r, row, dry)
		}
		res.OK = res.Error == ""
		if res.OK && !dry {
			created++
		}
		results = append(results, res)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"rows": results, "created": created})
}

// importUser checks (dry) or creates one user; it returns an error text
// and the password it generated.
func (s *Server) importUser(r *http.Request, row importRow, dry bool) (string, string) {
	var existing devpodv1alpha1.User
	switch err := s.Reader.Get(r.Context(), types.NamespacedName{Name: row.Username}, &existing); {
	case err == nil:
		return "user already exists", ""
	case !apierrors.IsNotFound(err):
		return err.Error(), ""
	}
	pw, generated := row.Password, ""
	if pw == "" {
		if dry {
			return "", ""
		}
		var err error
		if pw, err = generatePassword(generatedPasswordLen); err != nil {
			return err.Error(), ""
		}
		generated = pw
	}
	if len(pw) < s.PasswordMinLength {
		return fmt.Sprintf("password must be at least %d characters", s.PasswordMinLength), ""
	}
	if dry {
		return "", ""
	}
	hash, err := HashPassword(pw, s.PasswordMinLength)
	if err != nil {
		return err.Error(), ""
	}
	u := &devpodv1alpha1.User{
		ObjectMeta: metav1.ObjectMeta{Name: row.Username},
		Spec:       devpodv1alpha1.UserSpec{DisplayName: row.DisplayName, PasswordHash: hash},
	}
	if err := s.Client.Create(r.Context(), u); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return "user already exists", ""
		}
		return err.Error(), ""
	}
	return "", generated
}
