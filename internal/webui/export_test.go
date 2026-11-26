// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"github.com/mrhaoxx/devpod/internal/audit/store"

	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/access"
	"github.com/mrhaoxx/devpod/internal/agent"
	"github.com/mrhaoxx/devpod/internal/render"
)

// Handler accessors for the external test package. Test-only file.
func (s *Server) HandleCreateDevPodForTest() http.HandlerFunc { return s.handleCreateDevPod }
func (s *Server) HandleGetDevPodForTest() http.HandlerFunc    { return s.handleGetDevPod }
func (s *Server) HandleListDevPodsForTest() http.HandlerFunc  { return s.handleListDevPods }
func (s *Server) HandlePatchDevPodForTest() http.HandlerFunc  { return s.handlePatchDevPod }
func (s *Server) HandleDeleteDevPodForTest() http.HandlerFunc { return s.handleDeleteDevPod }
func (s *Server) HandleDevPodEventsForTest() http.HandlerFunc { return s.handleDevPodEvents }

func (s *Server) HandleWatchDevPodsForTest() http.HandlerFunc { return s.handleWatchDevPods }

func (s *Server) HandleMeForTest() http.HandlerFunc         { return s.handleMe }
func (s *Server) HandleGetPubkeysForTest() http.HandlerFunc { return s.handleGetPubkeys }
func (s *Server) HandlePutPubkeysForTest() http.HandlerFunc { return s.handlePutPubkeys }

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

// PlanLaunchForTest checks a launch as user without writing; the code
// is "" when the request as a whole is acceptable.
func (s *Server) PlanLaunchForTest(r *http.Request, user, config string, req LaunchRequest) (*LaunchPlan, string) {
	sess := Session{User: user}
	c, apiErr := s.visibleConfig(r.Context(), sess, config)
	if apiErr != nil {
		return nil, apiErr.Code
	}
	plan, apiErr := s.planLaunch(r, sess, c, req)
	if apiErr != nil {
		return nil, apiErr.Code
	}
	return plan, ""
}

// RunLaunchForTest writes a checked plan.
func (s *Server) RunLaunchForTest(r *http.Request, plan *LaunchPlan) ([]LaunchItem, int) {
	return s.runLaunch(r, Session{}, plan)
}

// DecideForTest is decide for a node of the given name and labels.
func (s *Server) DecideForTest(ctx context.Context, owner, cluster, node string, labels map[string]string) (access.Decision, bool, error) {
	return s.decide(ctx, owner, cluster, access.Node{Name: node, Labels: labels})
}

// LoginWaitForTest is how long a sign-in waits after n failures in a row.
func LoginWaitForTest(n int) time.Duration { return loginWait(n) }

// LoseLeadForTest: as when this replica stops leading.
func (s *Server) LoseLeadForTest() { s.loseLead() }

// PriorityCapForTest is priorityCap.
func (s *Server) PriorityCapForTest(ctx context.Context, user string, groups map[string]bool, cluster string) (int32, bool, error) {
	return s.priorityCap(ctx, user, groups, cluster)
}

// MaxTimeLimitForTest is maxTimeLimit.
func (s *Server) MaxTimeLimitForTest(ctx context.Context, cluster string) time.Duration {
	return s.maxTimeLimit(ctx, cluster)
}

func (s *Server) HandleSubmitJobForTest() http.HandlerFunc        { return s.handleSubmitJob }
func (s *Server) HandleListJobsForTest() http.HandlerFunc         { return s.handleListJobs }
func (s *Server) HandleGetJobForTest() http.HandlerFunc           { return s.handleGetJob }
func (s *Server) HandleCancelJobForTest() http.HandlerFunc        { return s.handleCancelJob }
func (s *Server) HandleResubmitJobForTest() http.HandlerFunc      { return s.handleResubmitJob }
func (s *Server) HandleJobLimitsForTest() http.HandlerFunc        { return s.handleJobLimits }
func (s *Server) HandleGetPriorityRulesForTest() http.HandlerFunc { return s.handleGetPriorityRules }
func (s *Server) HandlePutPriorityRulesForTest() http.HandlerFunc { return s.handlePutPriorityRules }

func (s *Server) HandleJobLogsForTest() http.HandlerFunc { return s.handleJobLogs }

// WriteJobStatusForTest writes j's status as the scheduler does.
func (s *Server) WriteJobStatusForTest(ctx context.Context, j *devpodv1alpha1.Job) bool {
	return s.writeJobStatus(ctx, j)
}

// SetQueueBeforeStartsForTest runs f in each round after the plan, before
// its jobs are started.
func (s *Server) SetQueueBeforeStartsForTest(f func()) { s.queueBeforeStarts = f }

// SetOtherUsageForTest makes f what other workloads hold on each machine
// of a cluster (the agent's word otherwise).
func (s *Server) SetOtherUsageForTest(f func(cluster string) map[string]agent.NodeUsage) {
	s.otherUsage = f
}

func (s *Server) RecordForTest(ctx context.Context, ev store.EventRow) { s.record(ctx, ev) }
func (s *Server) SetEventRetryForTest(d time.Duration)                 { s.eventRetry = d }
func (s *Server) EventFromForTest(r *http.Request, sess Session) store.EventRow {
	return eventFrom(r, sess)
}

func (s *Server) HandlePutDevPodYAMLForTest() http.HandlerFunc { return s.handlePutDevPodYAML }

// QueueAllowedForTest: the queue's view of the console's own cluster would
// give owner's entry — a DevPod with spec, or a one-rank job of spec — machine.
func (s *Server) QueueAllowedForTest(ctx context.Context, owner string, devpod bool, spec *corev1.PodSpec, machine string) bool {
	var e qEntry
	if devpod {
		dp := &devpodv1alpha1.DevPod{ObjectMeta: metav1.ObjectMeta{Name: "probe"}, Spec: devpodv1alpha1.DevPodSpec{Owner: owner, Pod: &devpodv1alpha1.PodWorkloadSpec{Spec: *spec}}}
		e = devPodEntry(dp, time.Now(), 0, "", "")
	} else {
		j := &devpodv1alpha1.Job{ObjectMeta: metav1.ObjectMeta{Name: "probe"}, Spec: devpodv1alpha1.JobSpec{Owner: owner, Nodes: 1,
			GPUsPerNode: int32(askOfPod(spec).GPU), Template: devpodv1alpha1.PodWorkloadSpec{Spec: *spec}}}
		e = jobEntry(j)
	}
	v, _, err := s.queueViewFor(ctx, s.kc(ctx), nil, []qEntry{e}, time.Now())
	return err == nil && v.Allowed(e, machine)
}

func (s *Server) HomeNodeForTest(ctx context.Context, dp *devpodv1alpha1.DevPod) string {
	return s.homeNode(ctx, s.kc(ctx), dp)
}

func (s *Server) HomePVCNameForTest(dp *devpodv1alpha1.DevPod) string { return render.HomePVCName(dp) }

func SetAllowedForTest(dp *devpodv1alpha1.DevPod, nodes []string) { access.SetAllowed(dp, nodes) }

func (s *Server) PinSweepRoundForTest(ctx context.Context) { s.pinSweepRound(ctx) }
