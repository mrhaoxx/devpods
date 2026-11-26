// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/audit/store"
	"github.com/mrhaoxx/devpod/internal/webui/clusters"
)

// A job's ranks write to their Pods' logs. While a rank runs its log is
// read live (through its cluster's agent); when the job ends each rank's
// is kept in the audit object store (as long as session recordings),
// before the ranks are deleted.

// maxRankLog bounds one rank's kept output.
const maxRankLog = 256 << 20

// rankContainer: the container a job's ranks run their command in.
func rankContainer(j *devpodv1alpha1.Job) string {
	if len(j.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	return j.Spec.Template.Spec.Containers[0].Name
}

// keepAndStop deletes j's ranks, keeping their output to its end under
// key(rank): each rank's log is opened, followed, before the ranks are
// told to stop, and read until its container is gone — what a rank says
// while shutting down (a checkpoint saved) is kept too. Best effort: a
// log that cannot be opened or kept is left out; ctx bounds it all.
// Without kept output it only deletes them.
func (s *Server) keepAndStop(ctx context.Context, cc *clusters.ClusterClients, j *devpodv1alpha1.Job, key func(rank int32) string) {
	streams := make([]io.ReadCloser, j.Spec.Nodes)
	// what it keeps with and where, as they are now (this runs on)
	podLogs, objects := s.PodLogs, store.ObjectStore(nil)
	if s.keepsRankLogs() {
		objects = s.Audit.Objects
		var wg sync.WaitGroup
		for r := int32(0); r < j.Spec.Nodes; r++ {
			wg.Add(1)
			go func() { // opened together: stopping waits for the slowest only
				defer wg.Done()
				rc, err := podLogs(ctx, j.Spec.Cluster, s.NS, rankName(j.Spec.Owner, j.Name, r), rankContainer(j), true)
				if err != nil {
					slog.Info("queue_logs", "job", j.Name, "rank", r, "err", err)
					return
				}
				streams[r] = rc
			}()
		}
		wg.Wait()
	}
	s.deleteRanks(ctx, cc, j)
	var wg sync.WaitGroup
	for r, rc := range streams {
		if rc == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = rc.Close() }()
			if err := objects.Put(ctx, key(int32(r)), io.LimitReader(rc, maxRankLog), -1); err != nil {
				slog.Warn("queue_logs", "job", j.Name, "rank", r, "err", err)
			}
		}()
	}
	wg.Wait()
}

// handleJobLogs serves GET /api/jobs/{id}/logs?rank=N[&follow=1]: a
// rank's output — live from its Pod while there (follow: as server-sent
// events, a line each), else what was kept. Its owner's and admins'.
func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFrom(w, r)
	if !ok {
		return
	}
	j, ok := s.jobFor(w, r, sess, true)
	if !ok {
		return
	}
	rank, err := strconv.Atoi(r.URL.Query().Get("rank"))
	if err != nil || rank < 0 || rank >= int(j.Spec.Nodes) {
		s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("rank is 0 to %d", j.Spec.Nodes-1), nil)
		return
	}
	// ?attempt=N: the output of an earlier start (one that did not end
	// the job: preempted, queued again), as kept
	if v := r.URL.Query().Get("attempt"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > int(j.Status.Attempts) {
			s.writeErr(w, http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf("attempt is 1 to %d", j.Status.Attempts), nil)
			return
		}
		if n < int(j.Status.Attempts) || (n == int(j.Status.Attempts) && j.Status.Phase == devpodv1alpha1.JobQueued) {
			s.keptLog(w, r, store.JobAttemptLogKey(j.Spec.Cluster, j.Name, int32(n), int32(rank)))
			return
		}
	}
	follow := r.URL.Query().Get("follow") == "1"
	if !jobEnded(j.Status.Phase) && s.PodLogs != nil {
		rc, err := s.PodLogs(r.Context(), j.Spec.Cluster, s.NS, rankName(j.Spec.Owner, j.Name, int32(rank)), rankContainer(j), follow)
		if err == nil {
			defer func() { _ = rc.Close() }()
			if follow {
				sseLines(w, rc)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.Copy(w, rc)
			return
		}
	}
	key := ""
	for _, rk := range j.Status.Ranks {
		if int(rk.Rank) == rank {
			key = rk.Log
		}
	}
	s.keptLog(w, r, key)
}

// keptLog answers with the kept output at key ("": none was kept).
func (s *Server) keptLog(w http.ResponseWriter, r *http.Request, key string) {
	if key == "" || s.Audit == nil || s.Audit.Objects == nil {
		s.writeErr(w, http.StatusNotFound, "NO_LOGS", "this rank's output is not there (not running, and none was kept)", nil)
		return
	}
	rc, err := s.Audit.Objects.Get(r.Context(), key)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "NO_LOGS", "this rank's kept output cannot be read: "+err.Error(), nil)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, rc)
}

// maxEventLine: a line of output longer than this goes as several events.
const maxEventLine = 64 << 10

// sseLines streams rc as server-sent events, one line each, until it or
// the caller ends. A line ends at \n, \r\n or a lone \r (a progress bar
// drawn again); one longer than maxEventLine goes in pieces — neither
// ends the stream.
func sseLines(w http.ResponseWriter, rc io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(rc, 64<<10)
	line := make([]byte, 0, 256)
	dirty := false // written, not flushed
	emit := func() bool {
		_, err := fmt.Fprintf(w, "data: %s\n\n", line)
		line, dirty = line[:0], true
		return err == nil
	}
	afterCR := false
	split := false // the line so far went out in pieces: its end sends nothing more
	for {
		if dirty && br.Buffered() == 0 && fl != nil {
			fl.Flush() // what has come is out before waiting for more
			dirty = false
		}
		b, err := br.ReadByte()
		if err != nil {
			if len(line) > 0 {
				emit()
			}
			if fl != nil {
				fl.Flush()
			}
			return
		}
		switch {
		case b == '\n' && afterCR: // the \n of a \r\n: the line went at the \r
		case b == '\n' || b == '\r':
			if (len(line) > 0 || !split) && !emit() {
				return
			}
			split = false
		default:
			line = append(line, b)
			if len(line) >= maxEventLine {
				if !emit() {
					return
				}
				split = true
			}
		}
		afterCR = b == '\r'
	}
}
