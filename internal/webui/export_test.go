// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	corev1 "k8s.io/api/core/v1"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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

func (s *Server) AdminForTest(username string, u *devpodv1alpha1.User) bool {
	return s.adminFor(username, u)
}

func (s *Server) HandleAuthConfigForTest() http.HandlerFunc     { return s.handleAuthConfig }
func (s *Server) HandlePasswordLoginForTest() http.HandlerFunc  { return s.handlePasswordLogin }
func (s *Server) HandleChangePasswordForTest() http.HandlerFunc { return s.handleChangePassword }
func (s *Server) HandleListUsersForTest() http.HandlerFunc      { return s.handleListUsers }
func (s *Server) HandleCreateUserForTest() http.HandlerFunc     { return s.handleCreateUser }
func (s *Server) HandlePatchUserForTest() http.HandlerFunc      { return s.handlePatchUser }
func (s *Server) HandleDeleteUserForTest() http.HandlerFunc     { return s.handleDeleteUser }

func (s *Server) HandleAdminListDevPodsForTest() http.HandlerFunc { return s.handleAdminListDevPods }

func (s *Server) HandleKoreTopologyForTest() http.HandlerFunc { return s.handleKoreTopology }

// KoreTransformForTest exercises the topology transform on synthetic CRs.
func KoreTransformForTest(items []map[string]any, devpodNS string) []nodeTopology {
	var list unstructured.UnstructuredList
	for _, it := range items {
		list.Items = append(list.Items, unstructured.Unstructured{Object: it})
	}
	return koreTopologyFromList(&list, devpodNS)
}

func (s *Server) HandleDevPodTopologyForTest() http.HandlerFunc { return s.handleDevPodTopology }

func (s *Server) FreshSessionForTest(w http.ResponseWriter, r *http.Request) (Session, bool) {
	return s.freshSession(w, r)
}

func (s *Server) HandleLiveDevPodsForTest() http.HandlerFunc  { return s.handleLiveDevPods }
func (s *Server) HandleLiveOverviewForTest() http.HandlerFunc { return s.handleLiveOverview }

func (s *Server) HandleDevPodMetricsForTest() http.HandlerFunc   { return s.handleDevPodMetrics }
func (s *Server) HandleDevPodInfoForTest() http.HandlerFunc      { return s.handleDevPodInfo }
func (s *Server) HandleAdminNodesForTest() http.HandlerFunc      { return s.handleAdminNodes }
func (s *Server) HandleAdminNodeForTest() http.HandlerFunc       { return s.handleAdminNode }
func (s *Server) HandleDevPodProcessesForTest() http.HandlerFunc { return s.handleDevPodProcesses }
func (s *Server) HandleUsersBatchForTest() http.HandlerFunc      { return s.handleUsersBatch }

func (s *Server) HandleImportUsersForTest() http.HandlerFunc { return s.handleImportUsers }

var ParseImportCSVForTest = parseImportCSV

func (s *Server) HandleSearchForTest() http.HandlerFunc { return s.handleSearch }

func (s *Server) HandleLiveRecentForTest() http.HandlerFunc { return s.handleLiveRecent }

// SetLiveHistoryBytesForTest sets the live view's history budget and
// returns a func restoring it.
func SetLiveHistoryBytesForTest(n int64) func() {
	old := liveHistoryBytes
	liveHistoryBytes = n
	return func() { liveHistoryBytes = old }
}

var SumRecentByNodeForTest = sumRecentByNode

func (s *Server) HandleRedeployDevPodForTest() http.HandlerFunc { return s.handleRedeployDevPod }

func (s *Server) HandleGetHTTPForTest() http.HandlerFunc    { return s.handleGetHTTP }
func (s *Server) HandlePutHTTPForTest() http.HandlerFunc    { return s.handlePutHTTP }
func (s *Server) HandleHTTPTicketForTest() http.HandlerFunc { return s.handleHTTPTicket }

func (s *Server) HandleCreateReservationForTest() http.HandlerFunc { return s.handleCreateReservation }
func (s *Server) HandlePatchReservationForTest() http.HandlerFunc  { return s.handlePatchReservation }
func (s *Server) HandleDeleteReservationForTest() http.HandlerFunc { return s.handleDeleteReservation }
func (s *Server) HandleListReservationsForTest() http.HandlerFunc  { return s.handleListReservations }
func (s *Server) HandleNodesForTest() http.HandlerFunc             { return s.handleNodes }
func (s *Server) HandleSetReservableForTest() http.HandlerFunc     { return s.handleSetReservable }

// FoldSnapshotLogForTest folds Job output lines into a progress event for sn.
func FoldSnapshotLogForTest(sn *devpodv1alpha1.DevPodSnapshot, lines []string, now time.Time) SnapshotProgress {
	f := &logFold{}
	for _, l := range lines {
		f.add(l)
	}
	return progressOf(sn, f, now)
}

// GuardUserPodSpecForTest returns the code and field path of the first
// violation ("" when the spec is allowed).
func GuardUserPodSpecForTest(spec *corev1.PodSpec, user string) (code, path string) {
	if e := guardUserPodSpec(spec, user); e != nil {
		d, _ := e.Detail.(map[string]string)
		return e.Code, d["path"]
	}
	return "", ""
}

var (
	NormalizeRegistryHostForTest = normalizeRegistryHost
	ImageRegistryHostForTest     = imageRegistryHost
)

// PrepareDevPodForTest runs the create path up to, not including, the
// write; it returns the error code, "" when the DevPod may be created.
func (s *Server) PrepareDevPodForTest(r *http.Request, user string, req CreateRequest, pending []*devpodv1alpha1.DevPod) (*devpodv1alpha1.DevPod, string) {
	dp, apiErr := s.prepareDevPod(r, Session{User: user}, req, pending)
	if apiErr != nil {
		return nil, apiErr.Code
	}
	return dp, ""
}
