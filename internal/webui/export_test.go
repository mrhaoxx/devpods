// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"net/http"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
)

// Handler accessors for the external test package. Test-only file.
func (s *Server) HandleCreateDevPodForTest() http.HandlerFunc { return s.handleCreateDevPod }
func (s *Server) HandleGetDevPodForTest() http.HandlerFunc    { return s.handleGetDevPod }
func (s *Server) HandleListDevPodsForTest() http.HandlerFunc  { return s.handleListDevPods }
func (s *Server) HandlePatchDevPodForTest() http.HandlerFunc  { return s.handlePatchDevPod }
func (s *Server) HandleDeleteDevPodForTest() http.HandlerFunc { return s.handleDeleteDevPod }
func (s *Server) HandleDevPodEventsForTest() http.HandlerFunc { return s.handleDevPodEvents }

func (s *Server) HandleWatchDevPodsForTest() http.HandlerFunc { return s.handleWatchDevPods }

func (s *Server) HandleMeForTest() http.HandlerFunc            { return s.handleMe }
func (s *Server) HandleGetPubkeysForTest() http.HandlerFunc    { return s.handleGetPubkeys }
func (s *Server) HandlePutPubkeysForTest() http.HandlerFunc    { return s.handlePutPubkeys }
func (s *Server) HandleListTemplatesForTest() http.HandlerFunc { return s.handleListTemplates }

func (s *Server) HandleDevPodStreamForTest() http.HandlerFunc { return s.handleDevPodStream }

func (s *Server) AdminForTest(username string, u *devpodv1alpha1.User) bool { return s.adminFor(username, u) }

func (s *Server) HandleAuthConfigForTest() http.HandlerFunc   { return s.handleAuthConfig }
func (s *Server) HandlePasswordLoginForTest() http.HandlerFunc { return s.handlePasswordLogin }
func (s *Server) HandleChangePasswordForTest() http.HandlerFunc { return s.handleChangePassword }
func (s *Server) HandleListUsersForTest() http.HandlerFunc    { return s.handleListUsers }
func (s *Server) HandleCreateUserForTest() http.HandlerFunc   { return s.handleCreateUser }
func (s *Server) HandlePatchUserForTest() http.HandlerFunc    { return s.handlePatchUser }
func (s *Server) HandleDeleteUserForTest() http.HandlerFunc   { return s.handleDeleteUser }

func (s *Server) HandleAdminListDevPodsForTest() http.HandlerFunc { return s.handleAdminListDevPods }
