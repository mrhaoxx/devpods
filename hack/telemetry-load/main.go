// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Command telemetry-load simulates N supervisors posting telemetry to the
// webui leader (audit-telemetry spec §14 item 0). Each fake uses the
// agent's own Buffer and Uploader, so buffering, resends and backoff are
// the real ones. With -webui it also watches /api/live/devpods and reports
// the sample → browser latency: fake 0 carries its sample time (unix ms)
// in netRx.
//
//	telemetry-load -n 400 -url http://…:8081/ingest/v1/batch -key-file key \
//	  -uids-file uids -duration 10m -webui http://10.0.0.5:8080 -user u -password-file pw
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrhaoxx/devpod/internal/telemetry/agent"
	"github.com/mrhaoxx/devpod/internal/telemetry/wire"
)

type counters struct {
	mu       sync.Mutex
	status   map[int]int
	errs     int
	failFrom map[string]time.Time // token → first failure of the current outage
	outages  []time.Duration
}

func (c *counters) record(tok string, status int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ok := err == nil && status/100 == 2
	if err != nil {
		c.errs++
	} else {
		c.status[status]++
	}
	switch start, failing := c.failFrom[tok]; {
	case !ok && !failing:
		c.failFrom[tok] = time.Now()
	case ok && failing:
		c.outages = append(c.outages, time.Since(start))
		delete(c.failFrom, tok)
	}
}

type countingTransport struct {
	next http.RoundTripper
	c    *counters
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	t.c.record(r.Header.Get("Authorization"), status, err)
	return resp, err
}

func quantiles(xs []time.Duration) (p50, p99, max time.Duration) {
	if len(xs) == 0 {
		return
	}
	s := append([]time.Duration(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(q float64) time.Duration { return s[min(len(s)-1, int(q*float64(len(s))))] }
	return at(0.5), at(0.99), s[len(s)-1]
}

// records is one realistic second: container, 2 GPUs, 1 RDMA port, 20
// active processes, and every 5 s the full process list.
func records(i int, seq uint64, now time.Time) []wire.Record {
	r := rand.Float64
	c := &wire.Container{TS: now, CPU: 2 + 4*r(), CPULimit: 8, MemWorkingSet: 6 << 30, MemAnon: 5 << 30, MemFile: 1 << 30,
		MemLimit: 32 << 30, IOReadBps: 1e6 * r(), IOWriteBps: 1e6 * r(), NetRx: 1e6 * r(), NetTx: 1e6 * r(),
		NetTCPRx: 9e5 * r(), NetTCPTx: 9e5 * r(), NetKind: "pod", Pids: 120}
	if i == 0 {
		c.NetRx = float64(now.UnixMilli())
	}
	recs := []wire.Record{{K: wire.KContainer, C: c}}
	for g := 0; g < 2; g++ {
		recs = append(recs, wire.Record{K: wire.KGPU, G: &wire.GPU{TS: now, Index: g, UUID: fmt.Sprintf("GPU-%04d-%d", i, g),
			Name: "NVIDIA A100-SXM4-80GB", Util: 100 * r(), MemUsed: 40000, MemTotal: 81920, Power: 300, Temp: 60, SMClock: 1410, ProcCount: 1}})
	}
	recs = append(recs, wire.Record{K: wire.KRDMA, R: &wire.RDMA{TS: now, IBDev: "mlx5_0", Port: 1, TxBps: 1e9 * r(), RxBps: 1e9 * r(),
		Scope: "vf_exclusive", Health: map[string]uint64{"port_rcv_errors": 0, "symbol_error": 0, "link_downed": 0}}})
	attr := &wire.Attr{Peer: fmt.Sprintf("10.1.%d.%d:41000", i/250, i%250), How: "tree"}
	for j := 0; j < 20; j++ {
		p := &wire.Proc{TS: now, Pid: 1000 + j, Start: uint64(j+1) * 100, CPU: r(), RSS: 200 << 20, IOR: 1e4 * r(), IOW: 1e4 * r(),
			TCPRx: 1e4 * r(), TCPTx: 1e4 * r(), Attr: attr}
		if j == 0 {
			p.GPU = []int{0, 1}
		}
		recs = append(recs, wire.Record{K: wire.KProc, P: p})
	}
	if seq%5 == 0 {
		full := &wire.FullProcs{TS: now}
		for j := 0; j < 60; j++ {
			full.Procs = append(full.Procs, wire.SnapProc{Pid: 1000 + j, Start: uint64(j+1) * 100, PPid: 1, Exe: "/usr/bin/python3",
				Cmdline: "python3 train.py --epochs 100", CPU: r(), RSS: 200 << 20, Attr: attr})
		}
		recs = append(recs, wire.Record{K: wire.KFullProcs, Full: full})
	}
	return recs
}

func watch(ctx context.Context, base, user, pwFile, name string, lat *[]time.Duration, mu *sync.Mutex) error {
	pw, err := os.ReadFile(pwFile)
	if err != nil {
		return err
	}
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]string{"username": user, "password": strings.TrimSpace(string(pw))})
	resp, err := hc.Post(base+"/api/auth/password", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("login: %s", resp.Status)
	}
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/live/devpods?scope=all", nil)
		resp, err := hc.Do(req)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		var last float64
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev struct {
				Items []struct {
					Name  string   `json:"name"`
					NetRx *float64 `json:"netRx"`
				} `json:"items"`
			}
			if json.Unmarshal([]byte(line[6:]), &ev) != nil {
				continue
			}
			now := float64(time.Now().UnixMilli())
			for _, it := range ev.Items {
				if it.Name == name && it.NetRx != nil && *it.NetRx != last {
					last = *it.NetRx
					if d := now - last; d >= 0 && d < 60000 {
						mu.Lock()
						*lat = append(*lat, time.Duration(d)*time.Millisecond)
						mu.Unlock()
					}
				}
			}
		}
		resp.Body.Close()
	}
	return nil
}

func main() {
	n := flag.Int("n", 400, "fake supervisors")
	url := flag.String("url", "", "ingest URL")
	keyFile := flag.String("key-file", "", "ingest HMAC key")
	uidsFile := flag.String("uids-file", "", `lines "uid name"; the first n are used`)
	duration := flag.Duration("duration", 10*time.Minute, "how long to post")
	drain := flag.Duration("drain", time.Minute, "after posting stops, wait this long for buffers to empty")
	webui := flag.String("webui", "", "webui base URL to watch /api/live/devpods (optional)")
	user := flag.String("user", "", "webui login (with -webui)")
	pwFile := flag.String("password-file", "", "webui password file (with -webui)")
	flag.Parse()

	key, err := os.ReadFile(*keyFile)
	must(err)
	key = bytes.TrimSpace(key)
	raw, err := os.ReadFile(*uidsFile)
	must(err)
	type fake struct{ uid, name string }
	var fakes []fake
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 {
			fakes = append(fakes, fake{f[0], f[1]})
		}
	}
	if len(fakes) < *n {
		must(fmt.Errorf("uids-file has %d entries, need %d", len(fakes), *n))
	}
	fakes = fakes[:*n]
	tokDir, err := os.MkdirTemp("", "telemetry-load")
	must(err)
	defer os.RemoveAll(tokDir)

	c := &counters{status: map[int]int{}, failFrom: map[string]time.Time{}}
	tr := countingTransport{next: &http.Transport{MaxIdleConnsPerHost: *n, IdleConnTimeout: time.Minute}, c: c}
	client := &http.Client{Timeout: 10 * time.Second, Transport: tr}
	ctx, cancel := context.WithCancel(context.Background())
	postCtx, stopPosting := context.WithTimeout(ctx, *duration)
	defer cancel()
	defer stopPosting()

	var seqs atomic.Uint64
	bufs := make([]*agent.Buffer, len(fakes))
	for i, f := range fakes {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(f.uid))
		tp := filepath.Join(tokDir, fmt.Sprint(i))
		must(os.WriteFile(tp, []byte(hex.EncodeToString(m.Sum(nil))), 0o600))
		buf := agent.NewBuffer(8 << 20)
		bufs[i] = buf
		up := &agent.Uploader{URL: func() string { return *url }, TokenPath: tp, Client: client}
		hdr := wire.Header{V: wire.Version, DevPodUID: f.uid, BootID: fmt.Sprintf("load-%d-%d", i, time.Now().Unix()), AgentVersion: "telemetry-load"}
		go up.Run(ctx, func() wire.Header { return hdr }, buf)
		go func(i int) {
			time.Sleep(time.Duration(rand.Int64N(int64(time.Second)))) // spread like real agents
			t := time.NewTicker(time.Second)
			defer t.Stop()
			var seq uint64
			for {
				select {
				case <-postCtx.Done():
					return
				case now := <-t.C:
					seq++
					seqs.Add(1)
					buf.Add(seq, records(i, seq, now.UTC()))
				}
			}
		}(i)
	}

	var lat []time.Duration
	var latMu sync.Mutex
	if *webui != "" {
		go func() {
			if err := watch(ctx, *webui, *user, *pwFile, fakes[0].name, &lat, &latMu); err != nil {
				fmt.Fprintln(os.Stderr, "watch:", err)
			}
		}()
	}

	report := func(tag string) {
		c.mu.Lock()
		st := fmt.Sprint(c.status)
		errs, outs := c.errs, append([]time.Duration(nil), c.outages...)
		c.mu.Unlock()
		latMu.Lock()
		l50, l99, lmax := quantiles(lat)
		nl := len(lat)
		latMu.Unlock()
		o50, _, omax := quantiles(outs)
		pending := 0
		for _, b := range bufs {
			pending += b.Len()
		}
		fmt.Printf("%s %s seconds=%d status=%s errors=%d outages=%d outage_p50=%v outage_max=%v unacked=%d live_samples=%d live_p50=%v live_p99=%v live_max=%v\n",
			time.Now().UTC().Format("15:04:05"), tag, seqs.Load(), st, errs, len(outs), o50.Round(time.Millisecond), omax.Round(time.Millisecond),
			pending, nl, l50, l99, lmax)
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
loop:
	for {
		select {
		case <-postCtx.Done():
			break loop
		case <-tick.C:
			report("progress")
		}
	}
	deadline := time.Now().Add(*drain)
	for time.Now().Before(deadline) {
		pending := 0
		for _, b := range bufs {
			pending += b.Len()
		}
		if pending == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	report("final")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "telemetry-load:", err)
		os.Exit(1)
	}
}
