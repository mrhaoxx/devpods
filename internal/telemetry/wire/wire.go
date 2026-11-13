// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package wire is the supervisor → webui telemetry protocol: one batch is
// a zstd-compressed NDJSON stream of a Header line followed by Records.
package wire

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Version is the protocol version carried in Header.V.
const Version = 1

// ErrTooLarge reports a batch whose decompressed size exceeds the limit.
var ErrTooLarge = errors.New("telemetry batch too large")

// Header identifies one batch: (DevPodUID, BootID, Seq) is unique.
type Header struct {
	V         int    `json:"v"`
	DevPodUID string `json:"devpod_uid"`
	BootID    string `json:"boot_id"`
	Seq       uint64 `json:"seq"`
	// FromSeq is the first second in this batch when an agent catching
	// up sends several at once (FromSeq..Seq); 0 means just Seq.
	FromSeq      uint64    `json:"from_seq,omitempty"`
	TS           time.Time `json:"ts"`
	AgentVersion string    `json:"agent_version,omitempty"`
}

// Kind tags a Record.
type Kind string

const (
	KContainer Kind = "c"
	KGPU       Kind = "g"
	KRDMA      Kind = "r"
	KProc      Kind = "p"
	KProcEvent Kind = "pe"
	KSSHConn   Kind = "sc"
	KCommand   Kind = "cmd"
	KInfo      Kind = "info"
	KSnapshot  Kind = "snap"
	KFullProcs Kind = "full"
	KAlert     Kind = "alert"
)

// Audit reports records that belong to the audit partition of the agent's
// buffer (spec §5.7): process events, ssh connections, commands, alerts.
func (k Kind) Audit() bool {
	switch k {
	case KProcEvent, KSSHConn, KCommand, KAlert:
		return true
	}
	return false
}

func (k Kind) known() bool {
	switch k {
	case KContainer, KGPU, KRDMA, KProc, KProcEvent, KSSHConn, KCommand, KInfo, KSnapshot, KFullProcs, KAlert:
		return true
	}
	return false
}

// Record is a tagged union: exactly the payload matching K is set.
type Record struct {
	K     Kind       `json:"k"`
	C     *Container `json:"c,omitempty"`
	G     *GPU       `json:"g,omitempty"`
	R     *RDMA      `json:"r,omitempty"`
	P     *Proc      `json:"p,omitempty"`
	PE    *ProcEvent `json:"pe,omitempty"`
	SC    *SSHConn   `json:"sc,omitempty"`
	Cmd   *Command   `json:"cmd,omitempty"`
	Info  *Info      `json:"info,omitempty"`
	Snap  *Snapshot  `json:"snap,omitempty"`
	Full  *FullProcs `json:"full,omitempty"`
	Alert *Alert     `json:"alert,omitempty"`
}

// Attr is a process's session attribution (spec §5.3): the sshd-session
// process it belongs to and the rule that decided it.
type Attr struct {
	ConnPid   int    `json:"conn_pid,omitempty"`
	ConnStart uint64 `json:"conn_start,omitempty"`
	// Peer is the sshd-session's TCP peer, i.e. the gateway's
	// backend_local_addr; the webui resolves the gateway session by it.
	Peer string `json:"peer,omitempty"`
	How  string `json:"how"` // tree | ssh_connection | sid_tty | inferred | unattributed | system | workload | untracked
}

// Container is one container-level sample (spec §6.1, §6.3).
type Container struct {
	TS            time.Time `json:"ts"`
	CPU           float64   `json:"cpu"`
	CPULimit      float64   `json:"cpu_limit"` // 0 = unlimited
	ThrottledUsec uint64    `json:"throttled_usec"`
	Cpuset        string    `json:"cpuset,omitempty"`
	MemWorkingSet uint64    `json:"mem_ws"`
	MemAnon       uint64    `json:"mem_anon"`
	MemFile       uint64    `json:"mem_file"`
	MemShmem      uint64    `json:"mem_shmem"`
	MemLimit      uint64    `json:"mem_limit"` // 0 = unlimited
	OOMKills      uint64    `json:"oom_kills"`
	IOReadBps     float64   `json:"io_r"`
	IOWriteBps    float64   `json:"io_w"`
	IOReadIops    float64   `json:"io_r_iops"`
	IOWriteIops   float64   `json:"io_w_iops"`
	NetRx         float64   `json:"net_rx"`
	NetTx         float64   `json:"net_tx"`
	NetTCPRx      float64   `json:"net_tcp_rx"`
	NetTCPTx      float64   `json:"net_tcp_tx"`
	NetOtherRx    float64   `json:"net_other_rx"`
	NetOtherTx    float64   `json:"net_other_tx"`
	NetRxPkts     float64   `json:"net_rx_pkts"`
	NetTxPkts     float64   `json:"net_tx_pkts"`
	NetErrs       float64   `json:"net_errs"`
	NetDrops      float64   `json:"net_drops"`
	NetKind       string    `json:"net_kind"` // pod | tcp-only
	Pids          uint64    `json:"pids"`
}

// GPU is one card's sample; -1 marks a field nvidia-smi reports as N/A.
type GPU struct {
	TS        time.Time `json:"ts"`
	Index     int       `json:"index"`
	UUID      string    `json:"uuid"`
	Name      string    `json:"name"`
	Util      float64   `json:"util"`
	MemUsed   float64   `json:"mem_used"`  // MiB
	MemTotal  float64   `json:"mem_total"` // MiB
	Power     float64   `json:"power"`     // W
	Temp      float64   `json:"temp"`      // °C
	SMClock   float64   `json:"sm_clock"`  // MHz
	ProcCount int       `json:"proc_count"`
	Shared    bool      `json:"shared"`
}

// RDMA is one IB port's sample (spec §6.4).
type RDMA struct {
	TS     time.Time         `json:"ts"`
	IBDev  string            `json:"ibdev"`
	Port   int               `json:"port"`
	TxBps  float64           `json:"tx_bps"`
	RxBps  float64           `json:"rx_bps"`
	TxPps  float64           `json:"tx_pps"`
	RxPps  float64           `json:"rx_pps"`
	Health map[string]uint64 `json:"health,omitempty"`
	Scope  string            `json:"scope"` // vf_exclusive | pf_shared
}

// Proc is one process's per-second sample (spec §6.2). Pid 0 is the
// "other" row summing every unreported process.
type Proc struct {
	TS    time.Time `json:"ts"`
	Pid   int       `json:"pid"`
	Start uint64    `json:"start"`
	CPU   float64   `json:"cpu"`
	RSS   uint64    `json:"rss"`
	IOR   float64   `json:"io_r"`
	IOW   float64   `json:"io_w"`
	TCPRx float64   `json:"tcp_rx"`
	TCPTx float64   `json:"tcp_tx"`
	GPU   []int     `json:"gpu,omitempty"`
	RDMA  bool      `json:"rdma,omitempty"`
	Attr  *Attr     `json:"attr,omitempty"`
}

// ProcEvent is a process_start, process_exit or untracked_entry (§5.2).
type ProcEvent struct {
	TS time.Time `json:"ts"`
	// FirstSeen is when the process was first seen; exit events repeat
	// it (and the start's ppid/exe/cmdline/uid) so they are complete rows.
	FirstSeen time.Time `json:"first_seen,omitzero"`
	Event     string    `json:"event"`
	Pid       int       `json:"pid"`
	Start     uint64    `json:"start"`
	PPid      int       `json:"ppid"`
	Exe       string    `json:"exe,omitempty"`
	Cmdline   string    `json:"cmdline,omitempty"`
	UID       int       `json:"uid"`
	Attr      *Attr     `json:"attr,omitempty"`
}

// SSHConn is a legit sshd-session and its peer address (§5.4).
type SSHConn struct {
	TS        time.Time `json:"ts"`
	FirstSeen time.Time `json:"first_seen,omitzero"`
	Pid       int       `json:"pid"`
	Start     uint64    `json:"start"`
	Peer      string    `json:"peer"`
	Closed    bool      `json:"closed,omitempty"`
}

// Command is one shell-hook command (§5.1).
type Command struct {
	TS         time.Time `json:"ts"`
	EventID    string    `json:"event_id"`
	ShellPid   int       `json:"shell_pid"`
	ShellStart uint64    `json:"shell_start"`
	TTY        string    `json:"tty,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	Cmd        string    `json:"cmd"`
	End        time.Time `json:"end"`
	Exit       int       `json:"exit"`
	Attr       *Attr     `json:"attr,omitempty"`
}

// GPUInfo and RDMAInfo describe the container's devices in Info.
type GPUInfo struct {
	UUID     string  `json:"uuid"`
	Name     string  `json:"name"`
	MemTotal float64 `json:"mem_total"`
	Driver   string  `json:"driver,omitempty"`
	CUDA     string  `json:"cuda,omitempty"`
}

type RDMAInfo struct {
	IBDev string `json:"ibdev"`
	Port  int    `json:"port"`
	Scope string `json:"scope"`
}

// Info is the container's static description (§6.5).
type Info struct {
	TS           time.Time         `json:"ts"`
	OS           string            `json:"os,omitempty"`
	Kernel       string            `json:"kernel,omitempty"`
	CPULimit     float64           `json:"cpu_limit"`
	Cpuset       string            `json:"cpuset,omitempty"`
	MemLimit     uint64            `json:"mem_limit"`
	GPUs         []GPUInfo         `json:"gpus,omitempty"`
	RDMA         []RDMAInfo        `json:"rdma,omitempty"`
	Shell        string            `json:"shell,omitempty"`
	AgentVersion string            `json:"agent_version,omitempty"`
	Collectors   map[string]string `json:"collectors,omitempty"` // name → ok | disabled: <why>
}

// SnapProc is one process line of a snapshot.
type SnapProc struct {
	Pid     int     `json:"pid"`
	Start   uint64  `json:"start"`
	PPid    int     `json:"ppid,omitempty"`
	Exe     string  `json:"exe,omitempty"`
	Cmdline string  `json:"cmdline,omitempty"`
	CPU     float64 `json:"cpu"`
	RSS     uint64  `json:"rss"`
	Attr    *Attr   `json:"attr,omitempty"`
}

type Listen struct {
	Port  int    `json:"port"`
	Proto string `json:"proto"`
	Addr  string `json:"addr,omitempty"`
	Pid   int    `json:"pid,omitempty"`
}

type Storage struct {
	Mount string `json:"mount"`
	Size  uint64 `json:"size"`
	Used  uint64 `json:"used"`
}

// Snapshot is the 15-second container overview (§6.5).
type Snapshot struct {
	TS      time.Time  `json:"ts"`
	TopCPU  []SnapProc `json:"top_cpu,omitempty"`
	TopRSS  []SnapProc `json:"top_rss,omitempty"`
	Procs   int        `json:"procs"`
	Threads int        `json:"threads"`
	Zombies int        `json:"zombies"`
	Listen  []Listen   `json:"listen,omitempty"`
	Storage []Storage  `json:"storage,omitempty"`
}

// FullProcs is the 5-second full process list (memory only, §5.10).
type FullProcs struct {
	TS    time.Time  `json:"ts"`
	Procs []SnapProc `json:"procs"`
}

// Alert is an agent-side alert: gap, decoder_error, collector failure.
type Alert struct {
	TS     time.Time `json:"ts"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail,omitempty"`
}

// Ack is the ingest response: every seq ≤ AckedSeq is durable.
type Ack struct {
	AckedSeq uint64 `json:"acked_seq"`
}

// Reused codecs: a fresh zstd encoder allocates its history window per
// call, which at one batch per second (and on the leader, one decode per
// DevPod per second) dominates the GC. A free list rather than a
// sync.Pool, which every GC cycle empties. Batches are small, so a
// 256 KiB window loses nothing.
const codecWindow = 256 << 10

var (
	encoders = make(chan *zstd.Encoder, 8)
	decoders = make(chan *zstd.Decoder, 32)
)

func getEncoder(w io.Writer) (*zstd.Encoder, error) {
	select {
	case zw := <-encoders:
		zw.Reset(w)
		return zw, nil
	default:
		return zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(codecWindow), zstd.WithLowerEncoderMem(true))
	}
}

func putEncoder(zw *zstd.Encoder) {
	zw.Reset(nil)
	select {
	case encoders <- zw:
	default:
	}
}

func getDecoder(r io.Reader) (*zstd.Decoder, error) {
	select {
	case zr := <-decoders:
		if err := zr.Reset(r); err != nil {
			zr.Close()
			return nil, err
		}
		return zr, nil
	default:
		// A frame declares its own window: without a cap a 34-byte request
		// makes the decoder allocate 512 MiB. Agents encode with 256 KiB.
		return zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(4*codecWindow), zstd.WithDecoderMaxMemory(64<<20))
	}
}

func putDecoder(zr *zstd.Decoder) {
	if err := zr.Reset(nil); err != nil {
		zr.Close()
		return
	}
	select {
	case decoders <- zr:
	default:
		zr.Close()
	}
}

// Encode writes h and recs as zstd-compressed NDJSON.
func Encode(w io.Writer, h Header, recs []Record) error {
	zw, err := getEncoder(w)
	if err != nil {
		return err
	}
	defer putEncoder(zw)
	enc := json.NewEncoder(zw)
	if err := enc.Encode(h); err != nil {
		_ = zw.Close()
		return err
	}
	for i := range recs {
		if err := enc.Encode(&recs[i]); err != nil {
			_ = zw.Close()
			return err
		}
	}
	return zw.Close()
}

// limited fails with ErrTooLarge once more than max bytes were read.
type limited struct {
	r   io.Reader
	n   int64
	max int64
}

func (l *limited) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.n > l.max {
		return n, ErrTooLarge
	}
	return n, err
}

// Decode reads one batch; records of unknown kinds are skipped. Each
// NDJSON line is unmarshalled once (a json.Decoder stream scans every
// value twice: to find its end, then to decode it).
func Decode(r io.Reader, maxBytes int64) (Header, []Record, error) {
	var n int64
	return decode(r, maxBytes, &n)
}

// DecodeN is Decode that also returns the decompressed size.
func DecodeN(r io.Reader, maxBytes int64) (Header, []Record, int64, error) {
	var n int64
	h, recs, err := decode(r, maxBytes, &n)
	return h, recs, n, err
}

func decode(r io.Reader, maxBytes int64, size *int64) (Header, []Record, error) {
	zr, err := getDecoder(r)
	if err != nil {
		return Header{}, nil, err
	}
	defer putDecoder(zr)
	lr := &limited{r: zr, max: maxBytes}
	defer func() { *size = lr.n }()
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 0, 64<<10), int(min(maxBytes+1, 1<<30)))
	// Over the limit, the scanner still returns the cut last line: the
	// size error wins over the JSON one.
	tooLarge := func(err error) bool {
		return lr.n > lr.max || errors.Is(err, ErrTooLarge) || errors.Is(err, bufio.ErrTooLong)
	}
	if !sc.Scan() {
		err := sc.Err()
		if tooLarge(err) {
			return Header{}, nil, ErrTooLarge
		}
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return Header{}, nil, fmt.Errorf("telemetry header: %w", err)
	}
	var h Header
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
		if tooLarge(err) {
			return Header{}, nil, ErrTooLarge
		}
		return Header{}, nil, fmt.Errorf("telemetry header: %w", err)
	}
	var recs []Record
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			if tooLarge(err) {
				return Header{}, nil, ErrTooLarge
			}
			return Header{}, nil, fmt.Errorf("telemetry record: %w", err)
		}
		if rec.K.known() {
			recs = append(recs, rec)
		}
	}
	if err := sc.Err(); err != nil {
		if tooLarge(err) {
			return Header{}, nil, ErrTooLarge
		}
		return Header{}, nil, fmt.Errorf("telemetry record: %w", err)
	}
	return h, recs, nil
}

// Token is a DevPod's ingest credential: hex(HMAC-SHA256(key, uid)).
func Token(key []byte, devpodUID string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(devpodUID))
	return hex.EncodeToString(m.Sum(nil))
}
