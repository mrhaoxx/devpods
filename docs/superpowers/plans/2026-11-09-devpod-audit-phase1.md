# DevPod Audit — Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship phase 1 of the audit spec: SSH-gateway session audit (raw-stream capture, exec/SFTP/scp/ZMODEM decoding, file content capture), optional hard policies, S3 + ClickHouse storage (external or bundled Silo/ClickHouse), webui audit API + pages, and the table-style DevPod list / tabbed detail page.

**Architecture:** A new `internal/audit` tree is wired into the gateway's existing proxy through a hook interface (`audit.SessionHooks`); the gateway records to a local spool and an uploader pushes segments/objects to S3 and row batches to ClickHouse. The webui reads ClickHouse, proxies S3, and enforces permissions server-side. Schema migrations, bucket creation and lifecycle rules run from a `devpod-webui migrate` Helm hook Job.

**Tech Stack:** Go 1.25, `golang.org/x/crypto/ssh`, `github.com/klauspost/compress/zstd`, `github.com/oklog/ulid/v2`, `github.com/ClickHouse/clickhouse-go/v2`, `github.com/aws/aws-sdk-go-v2` (s3 + feature/s3/manager), `github.com/testcontainers/testcontainers-go` (integration tests only); React 18 + TS + Tailwind v4, `asciinema-player` v3.

**Spec:** `docs/superpowers/specs/2026-11-09-devpod-audit-telemetry-design.md` (phase 1 = §3.2, §4, §7, §8 audit tables, §9 audit parts + list/detail layout, §10.1–10.2 audit parts, §11.1, §12, §13 一期).

## Global Constraints

- No Makefile; shell lives in `hack/`. License header on every Go file: `// Copyright 2026 The DevPod Authors.` + blank line + `// SPDX-License-Identifier: Apache-2.0`.
- After any `api/v1alpha1` change run `go generate ./...` and commit the regenerated deepcopy, `config/crd/bases/*` and `deploy/chart/crds/*`.
- Unit + envtest: `bash hack/test.sh` (`-race`). Integration (Docker): `bash hack/test-integration.sh` (`-tags integration`). Frontend: `cd web && npm test`.
- Audit off (no `--audit-clickhouse-url`, or `GatewayConfig.spec.audit.enabled=false`) ⇒ gateway behaviour identical to today.
- Session ID = ULID (`github.com/oklog/ulid/v2`), replacing `sid-<n>` everywhere.
- Raw segments: rotate at **8 MiB uncompressed or 60 s**, zstd, key `raw/<yyyy>/<mm>/<dd>/<session>/<seq 6-digit>.seg.zst`. File content key `files/<yyyy>/<mm>/<dd>/<session>/<channel>-<n>`. Dates are UTC of the session open time.
- Event batches flush every **5 s** and at session close.
- Spool default **20 GiB**; Warn ≥ 80 %, High ≥ 90 %, Full = content would eat into the event reserve (**max/20**).
- Policy defaults: everything Allow, FailMode Open. Enum strings: `Allow`/`Deny`, `Open`/`Closed`.
- User-visible copy (exact): `blocked by DevPod audit policy: downloads disabled` · `blocked by DevPod audit policy: uploads disabled` · `blocked by DevPod audit policy: file exceeds size limit` · `blocked by DevPod audit policy: port forwarding disabled` · `blocked by DevPod audit policy: agent forwarding disabled` · `blocked by DevPod audit policy: X11 forwarding disabled` · `audit storage unavailable (fail-closed policy)` · default banner notice `本会话将被记录用于安全审计`. Warning: `download is denied but local port forwarding is allowed: VS Code can bypass the download restriction`.
- Storage names: bucket `devpod-audit`, ClickHouse database `devpod_audit`, accounts `devpod_gateway` / `devpod_webui` / `devpod_migrate`. Retention default **180 days** (`audit.retentionDays`).
- Credentials directories: S3 dir holds files `access-key`, `secret-key`; ClickHouse dir holds `username`, `password`.
- Gateway pod: `terminationGracePeriodSeconds: 300`, session grace `60s`, drain timeout `220s`.
- Webui audit API: time range defaults to last 7 days; `limit` default 100, max 1000; cursor pagination; ClickHouse `max_execution_time=10`. New error codes `AUDIT_DISABLED`. Access-log actions: `view_session`, `replay`, `download_file`, `download_raw`, `edit_policy`.
- Non-admins only ever see sessions they initiated; audit handlers re-resolve admin per request (never trust the cookie bit).

## Review Focus

- Pipelined SFTP (OpenSSH `-R 64`, many outstanding READs answered out of order) — content must reassemble byte-exact and the pump must never buffer more than one packet → `TestSFTP_PipelinedFixture` in Task 7.
- S3/ClickHouse unreachable when a session starts (fail-open) — the session must work normally; data piles up in the spool and becomes gaps only when full → `TestRecorder_FailOpenWhenStoresDown` in Task 13.
- Binary exec output that happens to contain a ZMODEM header (`**\x18B00`) must pass through untouched — ZMODEM decoding only applies to pty channels → `TestSession_ZmodemOnlyOnPty` in Task 13.
- Multi-byte UTF-8 split across records, and invalid bytes, in TTY output — replay must show the character, never break the cast JSON → `TestWriteCast_UTF8SplitAndInvalid` in Task 5.
- ControlMaster-style many concurrent channels on one connection — no races, distinct channel IDs, all rows present → `TestSession_ConcurrentChannelsRace` in Task 13.

---

## File Structure

| Action | Path | Responsibility |
|---|---|---|
| Create | `api/v1alpha1/audit_types.go` (+`_test`) | `AuditSpec`, `AuditPolicy`, `ForwardingPolicy`, enums |
| Modify | `api/v1alpha1/gatewayconfig_types.go`, `user_types.go` | `GatewayConfigSpec.Audit`, `UserSpec.AuditPolicy` |
| Create | `internal/audit/policy/` | `Effective`, `Resolve`, `Warnings` |
| Create | `internal/audit/sshmsg/` | SSH request / channel-extra payload parsing |
| Create | `internal/audit/rawlog/` | record framing, zstd segments, iterators, asciicast conversion |
| Create | `hack/audit-fixture/`, `hack/gen-audit-fixtures.sh`, `internal/audit/decode/testdata/` | real-client protocol fixtures |
| Create | `internal/audit/decode/` | `Decoder` contract + SFTP, legacy scp, ZMODEM decoders/filters |
| Create | `internal/audit/store/` | rows, key builders, S3 object store, ClickHouse writer/reader, embedded migrations |
| Create | `internal/audit/spool/` | local spool accounting + uploader |
| Create | `internal/audit/` (`hooks.go`, `recorder.go`, `session.go`, `channel.go`, `classify.go`, `metrics.go`) | gateway-side recorder implementing the hooks |
| Modify | `internal/gateway/proxy.go` (+`_test`) | hook points: channel open, requests, data filter, Done |
| Modify | `cmd/gateway/main.go` (+`_test`) | flags, recorder, per-session policy/banner, ULID, shutdown drain |
| Modify | `cmd/webui/main.go` | audit flags, `migrate` subcommand |
| Create | `internal/webui/audit.go`, `api_audit.go` (+`_test`) | audit deps, fresh-admin helper, audit API |
| Modify | `internal/webui/api_users.go`, `api_devpods.go` | `features.audit`; pod info on detail |
| Create | `web/src/format.ts`, `web/src/columns.ts`, `web/src/audit.ts` (+tests) | pure helpers |
| Create | `web/src/components/DevPodTable.tsx`, `CastPlayer.tsx`, `AuditPolicyEditor.tsx` | UI components |
| Create | `web/src/pages/audit/*.tsx` | audit pages |
| Modify | `web/src/pages/PodList.tsx`, `AdminDevPods.tsx`, `PodDetail.tsx`, `AdminUsers.tsx`, `ui.tsx`, `main.tsx`, `api.ts` | new layout, routes, nav |
| Create | `deploy/chart/templates/audit-s3.yaml`, `audit-clickhouse.yaml`, `audit-migrate.yaml`, `_audit.tpl` | bundled storage + migrate Job |
| Modify | `deploy/chart/values.yaml`, `templates/gateway.yaml`, `webui.yaml`, `gatewayconfig.yaml` | audit wiring |
| Create | `hack/test-integration.sh`, `hack/test-chart.sh`, `hack/e2e-audit.sh`, `test/e2e/images/audit-box/Dockerfile` | test entry points |
| Modify | `.github/workflows/ci.yaml`, `QUICKSTART.md` | CI + docs |

---

### Task 1: API types for audit config and per-user policy

**Files:**
- Create: `api/v1alpha1/audit_types.go`, `api/v1alpha1/audit_types_test.go`
- Modify: `api/v1alpha1/gatewayconfig_types.go` (add field to `GatewayConfigSpec`), `api/v1alpha1/user_types.go` (add field to `UserSpec`)
- Regen: `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/devpod.io_{gatewayconfigs,users}.yaml`, `deploy/chart/crds/*`

**Interfaces:**
- Produces: the Go types exactly as in spec §10.1 — `AuditSpec{Enabled bool; Notice string; Policy AuditPolicy}`, `AuditPolicy{Download, Upload *PolicyAction; MaxFileSize *resource.Quantity; Forwarding *ForwardingPolicy; AgentForwarding, X11Forwarding *PolicyAction; FailMode *FailMode}`, `ForwardingPolicy{Local, Remote *PolicyAction; RecordContent *bool}`, `PolicyAction` (`PolicyAllow="Allow"`, `PolicyDeny="Deny"`), `FailMode` (`FailOpen="Open"`, `FailClosed="Closed"`); `GatewayConfigSpec.Audit *AuditSpec \`json:"audit,omitempty"\``; `UserSpec.AuditPolicy *AuditPolicy \`json:"auditPolicy,omitempty"\``.

- [ ] **Step 1: Write the failing tests**

```go
func TestAuditPolicy_EmptyMarshalsToEmptyObject(t *testing.T) {
	b, _ := json.Marshal(AuditPolicy{})
	if string(b) != "{}" { t.Fatalf("got %s", b) }
}
func TestAuditPolicy_RoundTrip(t *testing.T) {
	deny := PolicyDeny; closed := FailClosed; q := resource.MustParse("100Mi"); rec := true
	in := AuditPolicy{Download: &deny, MaxFileSize: &q, FailMode: &closed,
		Forwarding: &ForwardingPolicy{Local: &deny, RecordContent: &rec}}
	// marshal → unmarshal → reflect.DeepEqual(in, out) (compare MaxFileSize via .Cmp)
}
func TestCRD_AuditEnumsPresent(t *testing.T) {
	// read ../../config/crd/bases/devpod.io_users.yaml and devpod.io_gatewayconfigs.yaml;
	// assert each contains "auditPolicy:"/"audit:" and the enum lists "- Allow", "- Deny", "- Open", "- Closed".
}
```

- [ ] **Step 2: Run** `go test ./api/v1alpha1/ -run 'Audit|CRD_Audit' -v` — Expected: FAIL (undefined types).
- [ ] **Step 3: Add the types** in `audit_types.go` with `+kubebuilder:validation:Enum=Allow;Deny` on `PolicyAction` and `Enum=Open;Closed` on `FailMode`; godoc on every field (spec §7.1 semantics, one line each). Add the two fields to the existing specs.
- [ ] **Step 4: Regenerate** `go generate ./...` then `go test ./api/v1alpha1/ -v` — Expected: PASS; `git status` shows regenerated CRDs in both `config/crd/bases` and `deploy/chart/crds`.
- [ ] **Step 5: Commit** `git add api config deploy/chart/crds && git commit -m "api: GatewayConfig.spec.audit and User.spec.auditPolicy"`

---

### Task 2: Policy resolution

**Files:**
- Create: `internal/audit/policy/policy.go`, `internal/audit/policy/policy_test.go`

**Interfaces:**
- Consumes: Task 1 types.
- Produces:
```go
type Effective struct {
	DenyDownload, DenyUpload           bool
	MaxFileSize                        int64 // bytes; 0 = unlimited
	DenyLocalForward, DenyRemoteForward bool
	RecordForwardContent               bool
	DenyAgentForward, DenyX11          bool
	FailClosed                         bool
}
func Resolve(def, user *devpodv1alpha1.AuditPolicy) Effective // field-wise: user field set ⇒ wins; nil ⇒ default; both nil ⇒ allow
func (e Effective) HasDeny() bool                                // any of DenyDownload/DenyUpload/MaxFileSize>0
func (e Effective) Warnings() []string
const WarnVSCodeBypass = "download is denied but local port forwarding is allowed: VS Code can bypass the download restriction"
```

- [ ] **Step 1: Write the failing tests** — `TestResolve_NilNilAllowsEverything` (`Resolve(nil,nil) == Effective{}`); `TestResolve_UserOverridesField` (def Download=Deny, user Download=Allow ⇒ `!DenyDownload`; def Upload=Deny untouched ⇒ `DenyUpload`); `TestResolve_ForwardingSubfieldsMergeIndependently` (def Forwarding{Local:Deny}, user Forwarding{Remote:Deny} ⇒ both deny); `TestResolve_MaxFileSize` (`"100Mi"` ⇒ `104857600`); `TestWarnings_VSCodeBypass` (DenyDownload && !DenyLocalForward ⇒ `[]string{WarnVSCodeBypass}`; with DenyLocalForward ⇒ empty).
- [ ] **Step 2: Run** `go test ./internal/audit/policy/ -v` — Expected: FAIL.
- [ ] **Step 3: Implement** `Resolve`, `HasDeny`, `Warnings`.
- [ ] **Step 4: Run** the same command — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "audit: policy resolution (cluster default + per-user override)"`

---

### Task 3: Raw-stream records and zstd segments

**Files:**
- Create: `internal/audit/rawlog/record.go`, `segment.go`, `record_test.go`, `segment_test.go`
- Modify: `go.mod`/`go.sum` (`github.com/klauspost/compress`)

**Interfaces:**
- Produces:
```go
type Kind uint8
const (
	KindChannelOpen Kind = iota + 1 // payload: JSON ChannelOpen
	KindRequest                     // payload: JSON Request
	KindDataIn                      // raw bytes client→backend
	KindDataOut                     // raw bytes backend→client (stdout)
	KindDataErr                     // raw bytes backend→client (stderr)
	KindInjectToClient              // raw bytes the gateway wrote to the client
	KindInjectToBackend             // raw bytes the gateway wrote to the backend
	KindChannelClose                // payload: JSON ChannelClose
	KindGap                         // payload: JSON Gap
)
type Record struct { TS time.Time; Channel uint32; Kind Kind; Payload []byte }
type ChannelOpen struct { Dir uint8 `json:"dir"`; Type string `json:"type"`; Extra []byte `json:"extra,omitempty"` }
type Request struct { Dir uint8 `json:"dir"`; Type string `json:"type"`; WantReply bool `json:"want"`; OK bool `json:"ok"`; Denied bool `json:"denied,omitempty"`; Payload []byte `json:"payload,omitempty"` }
type ChannelClose struct { BytesIn int64 `json:"in"`; BytesOut int64 `json:"out"` }
type Gap struct { Bytes int64 `json:"bytes"`; Reason string `json:"reason"` }
func Append(dst []byte, r Record) []byte          // uvarint(unix nanos) uvarint(channel) byte(kind) uvarint(len) payload
func NewReader(r io.Reader) *Reader                 // (*Reader).Next() (Record, error); io.EOF at clean end, io.ErrUnexpectedEOF on truncation
const Magic = "DPRAW1\n"                            // first bytes of every decompressed segment
type SegmentConfig struct {
	MaxBytes int64; MaxAge time.Duration
	Create   func(seq uint32) (io.WriteCloser, error)
	OnSeal   func(seq uint32, first, last time.Time, rawBytes int64) error
	Now      func() time.Time
}
func NewSegmentWriter(cfg SegmentConfig) *SegmentWriter // Write(Record) error; Tick() error; Close() error
func OpenSegment(r io.Reader) (*Reader, error)          // zstd-decode + Magic check
type Iterator interface { Next() (Record, error) }
func Concat(n int, open func(i int) (io.ReadCloser, error)) Iterator // segments in order, closes each
```

- [ ] **Step 1: Write the failing tests** — `TestRecord_RoundTripAllKinds` (one record of each kind, `Append` then `Reader.Next` equal incl. TS to the nanosecond); `TestReader_Truncated` (drop last byte ⇒ `io.ErrUnexpectedEOF`); `TestSegment_RotatesAtMaxBytes` (MaxBytes 8 MiB; write three 5 MiB DataOut records ⇒ `OnSeal` called for seq 1 after the 2nd record, seq 2 on `Close`); `TestSegment_RotatesOnAgeViaTick` (fake Now advances 61 s ⇒ `Tick` seals); `TestSegment_EachDecodesIndependently` (open seq 2 alone with `OpenSegment` ⇒ first record is the 3rd written); `TestConcat_PreservesOrder`.
- [ ] **Step 2: Run** `go test ./internal/audit/rawlog/ -v` — Expected: FAIL.
- [ ] **Step 3: Implement.** Writer seals when uncompressed bytes ≥ MaxBytes *after* a write, or on `Tick` when `Now()-segmentStart ≥ MaxAge` and the segment is non-empty. Each segment = new zstd encoder writing `Magic` first.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "audit/rawlog: record framing and zstd segments"`

---

### Task 4: SSH payload parsing

**Files:**
- Create: `internal/audit/sshmsg/sshmsg.go`, `sshmsg_test.go`

**Interfaces:**
- Produces:
```go
type PtyReq struct { Term string; Cols, Rows uint32 }
func ParsePtyReq(p []byte) (PtyReq, error)
func ParseExec(p []byte) (string, error)
func ParseSubsystem(p []byte) (string, error)
func ParseWindowChange(p []byte) (cols, rows uint32, err error)
func ParseExitStatus(p []byte) (uint32, error)
func ParseExitSignal(p []byte) (string, error)
func ParseEnv(p []byte) (name, value string, err error)
type TCPIP struct { Host string; Port uint32; OrigHost string; OrigPort uint32 }
func ParseTCPIPExtra(extra []byte) (TCPIP, error)            // direct-tcpip and forwarded-tcpip share the layout
func ParseTCPIPForward(p []byte) (host string, port uint32, err error)
```

- [ ] **Step 1: Write the failing tests** — one table test per parser: build the payload with `ssh.Marshal` of the RFC 4254 struct (e.g. `struct{Term string; Cols, Rows, W, H uint32; Modes string}` for pty-req), parse, compare; plus a `TestParse_Malformed` row per parser (`[]byte{0x00}` ⇒ error).
- [ ] **Step 2: Run** `go test ./internal/audit/sshmsg/ -v` — Expected: FAIL.
- [ ] **Step 3: Implement** with `ssh.Unmarshal` into local structs.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "audit/sshmsg: RFC 4254 payload parsers"`

---

### Task 5: asciicast v2 conversion

**Files:**
- Create: `internal/audit/rawlog/cast.go`, `cast_test.go`, `testdata/cast/simple.cast` (golden)

**Interfaces:**
- Consumes: Task 3 `Iterator`, `Record`; Task 4 `ParsePtyReq`, `ParseWindowChange`.
- Produces: `func WriteCast(w io.Writer, it Iterator, channel uint32) error` — header `{"version":2,"width":W,"height":H,"timestamp":<unix s of ChannelOpen>,"env":{"TERM":T}}` (80×24, no TERM when the channel has no pty-req); events `[t,"o",s]` for DataOut+DataErr+InjectToClient, `[t,"i",s]` for DataIn, `[t,"r","WxH"]` for window-change; `t` = seconds since the channel's ChannelOpen, 6 decimals. Streams record-by-record (no full buffering).

- [ ] **Step 1: Write the failing tests** — `TestWriteCast_Golden` (synthesized stream: open, pty-req 120×40 `xterm-256color`, shell, out `hello\r\n`, in `ls\r`, window-change 100×30, out `a b\r\n`; compare with `testdata/cast/simple.cast`; `-update` flag rewrites the golden); `TestWriteCast_UTF8SplitAndInvalid` (out `"\xe4\xbd"` then `"\xa0"` then `"\xff"` ⇒ events decode to `"你"` and `"�"`, every line is valid JSON); `TestWriteCast_OtherChannelsExcluded`; `TestWriteCast_NoPtyDefaults80x24`.
- [ ] **Step 2: Run** `go test ./internal/audit/rawlog/ -run Cast -v` — Expected: FAIL.
- [ ] **Step 3: Implement.** Keep a per-stream (o / i) carry buffer for an incomplete trailing UTF-8 sequence (≤ 3 bytes); replace invalid bytes with U+FFFD via `strings.ToValidUTF8` on the completed part.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "audit/rawlog: asciicast v2 conversion"`

---

### Task 6: Real-client protocol fixtures

**Files:**
- Create: `hack/audit-fixture/main.go`, `hack/gen-audit-fixtures.sh`, `hack/fake-ssh.sh`
- Create (generated, committed): `internal/audit/decode/testdata/{sftp-put-get,sftp-pipelined,scp-upload,scp-download,scp-recursive,zmodem-sz,zmodem-rz}.raw` and a `.json` sidecar per file

**Interfaces:**
- Produces: uncompressed rawlog streams (Task 3 format, channel 1) — ChannelOpen(`session`), the Request(s) given by flags, then DataIn/DataOut in arrival order — and sidecars `{"files":[{"direction":"upload|download","path":"...","size":N,"sha256":"..."}]}`.
- CLI: `audit-fixture -o OUT.raw [-pty TERM] [-request 'subsystem:sftp'|'exec:<cmd>'|'shell'] -- CMD ARGS...` runs CMD; its own stdin → CMD stdin is recorded as DataIn, CMD stdout → own stdout as DataOut.

- [ ] **Step 1: Write `hack/audit-fixture/main.go`** (no test; it is a dev tool) and `hack/gen-audit-fixtures.sh` which builds it to `bin/audit-fixture` and produces, in a temp dir with deterministic content (`openssl enc -aes-128-ctr -nosalt -K 00..00 -iv 00..00 < /dev/zero | head -c N`):
  - `sftp-put-get`: `sftp -b batch -D "bin/audit-fixture -o … -request subsystem:sftp -- /usr/lib/openssh/sftp-server"`; batch = mkdir, put 3 MiB file, get it back, rename, rm.
  - `sftp-pipelined`: same wrapper, `sftp -B 4096 -R 64`, put + get a 2 MiB file.
  - `scp-upload` / `scp-download` / `scp-recursive`: `scp -O -S hack/fake-ssh.sh` where the fake ssh script execs `bin/audit-fixture -request "exec:$*" -- $*` (drop the host argument); 1 MiB file; `-r` over a 3-file tree.
  - `zmodem-sz` / `zmodem-rz`: `socat EXEC:"bin/audit-fixture -pty xterm -request shell -- sz -q f" EXEC:"rz -q -y"` (and the reverse) on a 1 MiB file.
  The script writes each sidecar from `sha256sum`/`stat`. Required host tools: `openssh-client`, `openssh-sftp-server`, `lrzsz`, `socat`, `openssl`.
- [ ] **Step 2: Run** `bash hack/gen-audit-fixtures.sh` — Expected: 7 `.raw` + 7 `.json` files; total < 12 MiB; `go run ./hack/audit-fixture -h` prints usage.
- [ ] **Step 3: Commit** `git add hack/audit-fixture hack/gen-audit-fixtures.sh hack/fake-ssh.sh internal/audit/decode/testdata && git commit -m "audit: real-client SFTP/scp/ZMODEM fixtures"`

---

### Task 7: Decoder contract and SFTP decoder/filter

**Files:**
- Create: `internal/audit/decode/decode.go`, `sftp.go`, `sftp_test.go`, `helpers_test.go`

**Interfaces:**
- Consumes: Task 2 `policy.Effective`; Task 3 `rawlog.NewReader` (fixtures); Task 6 fixtures.
- Produces:
```go
type Dir uint8
const ( ClientToBackend Dir = 0; BackendToClient Dir = 1 )
type Result struct {
	Pass  []byte // forward in the same direction
	Reply []byte // write back to the sender
	CloseChannel bool
}
type Decoder interface { Feed(dir Dir, p []byte) Result; Close(clean bool) }
type FileMeta struct { Protocol, Direction, Path string; DeclaredSize int64 } // Protocol sftp|scp|zmodem; Direction upload|download; -1 unknown size
type FileWriter interface { WriteAt(p []byte, off int64) (int, error); Finish(complete bool) }
type EventKind uint8
const ( EventFileOp EventKind = iota + 1; EventPolicy; EventDecoderError )
type Event struct { Kind EventKind; Protocol, Op, Path, Path2, Rule, Action, Detail string }
type Sink interface { OpenFile(m FileMeta) FileWriter; Emit(e Event) }
const (
	MsgDownloadDenied = "blocked by DevPod audit policy: downloads disabled"
	MsgUploadDenied   = "blocked by DevPod audit policy: uploads disabled"
	MsgFileTooLarge   = "blocked by DevPod audit policy: file exceeds size limit"
	MaxSFTPPacket     = 1 << 20
)
func NewSFTP(pol policy.Effective, sink Sink) Decoder
```
- Contract (doc comment on `Decoder`): `Pass` and `Reply` contain only whole protocol units; the proxy writes each under the destination's write mutex, so injected units never interleave inside another unit.

- [ ] **Step 1: Write the failing tests** (helpers: `feedFixture(t, d, path, chunk int) (passC2B, passB2C []byte)`, a recording `fakeSink`):
  - `TestSFTP_PutGetFixture` — for chunk ∈ {1, 7, 4096, 65536}: sink got one upload + one download, `sha256` equals sidecar, `complete=true`; `EventFileOp` ops include `mkdir`, `rename` (Path2 set), `remove`; with zero policy, concatenated Pass per direction == fixture bytes per direction.
  - `TestSFTP_PipelinedFixture` — `sftp-pipelined.raw` at chunk 65536: both files match sidecar sha256; for every Feed whose input ends on a packet boundary, `len(Pass) == len(input)+carried` and the decoder's carry is 0 afterwards (expose `carry()` in an `export_test.go`).
  - `TestSFTP_DenyDownload` — OPEN with pflags READ (0x1) ⇒ `Pass` empty, `Reply` parses as `SSH_FXP_STATUS` with the same request id, code 3, message `MsgDownloadDenied`; one `EventPolicy{Rule:"download",Action:"deny"}`. Same for pflags READ|WRITE.
  - `TestSFTP_DenyUpload` — OPEN with WRITE|CREAT ⇒ status code 3, `MsgUploadDenied`, `Rule:"upload"`.
  - `TestSFTP_MaxFileSize` — MaxFileSize 1024: WRITE (off 0, 1000 B) passes; WRITE (off 1000, 100 B) ⇒ status code 4, `MsgFileTooLarge`, `Rule:"maxFileSize"`; READ with off+len > 1024 likewise.
  - `TestSFTP_TruncatedTransfer` — feed `sftp-put-get.raw` up to the middle of the upload's WRITEs, `Close(false)` ⇒ that file's `Finish(false)`.
  - `TestSFTP_OversizePacketDisablesDecoder` — length header 2 MiB ⇒ one `EventDecoderError`, all later bytes pass through verbatim.
- [ ] **Step 2: Run** `go test ./internal/audit/decode/ -run SFTP -v` — Expected: FAIL.
- [ ] **Step 3: Implement `NewSFTP`.** Packetize both directions (carry partial packets; cap `MaxSFTPPacket`). Track `handle → {path, direction, writer, covered intervals}` from OPEN/HANDLE, `request id → {handle, offset}` for READ so DATA responses land at the right offset. Download = OPEN with READ flag; upload = WRITE flag; both ⇒ both. Policy denials synthesize `SSH_FXP_STATUS` (v3 layout: id, code, message, lang ""). On CLOSE of a handle call `Finish(covered == [0,max))`. Ops: REMOVE→`remove`, RENAME→`rename`, MKDIR→`mkdir`, RMDIR→`rmdir`, SETSTAT/FSETSTAT→`setstat`. Mutex around shared state (Feed is called from two goroutines).
- [ ] **Step 4: Run** — Expected: PASS (also with `-race`).
- [ ] **Step 5: Commit** `git commit -m "audit/decode: SFTP decoder with content capture and policy filter"`

---

### Task 8: Legacy scp decoder/filter

**Files:**
- Create: `internal/audit/decode/scp.go`, `scp_test.go`

**Interfaces:**
- Consumes: Task 7 contract.
- Produces:
```go
type SCPMode uint8
const ( SCPSink SCPMode = iota + 1 /* scp -t: client uploads */; SCPSource /* scp -f: client downloads */ )
func ParseSCPCommand(cmd string) (SCPMode, bool) // argv[0] basename "scp" with -t or -f among flags (-r -p -d -v -q accepted)
func NewSCP(mode SCPMode, pol policy.Effective, sink Sink) Decoder
```

- [ ] **Step 1: Write the failing tests** — `TestParseSCPCommand` table (`"scp -t /tmp"`→Sink, `"scp -r -f a"`→Source, `"/usr/bin/scp -p -t ."`→Sink, `"ls -t"`→false, `"scp"`→false); `TestSCP_UploadFixture`, `TestSCP_DownloadFixture`, `TestSCP_RecursiveFixture` (chunks {1, 4096}; files and sha256 match sidecars; paths joined with the `D` directory stack); `TestSCP_MaxFileSizeUpload` (Sink, `C0644 5000 f` with max 1024 ⇒ the C line is not passed, `Reply == "\x01" + MsgFileTooLarge + "\n"`, `CloseChannel`); `TestSCP_MaxFileSizeDownload` (Source ⇒ `Pass` to client is the error line, `Reply` to backend is the same error line, `CloseChannel`).
- [ ] **Step 2: Run** `go test ./internal/audit/decode/ -run SCP -v` — Expected: FAIL.
- [ ] **Step 3: Implement** the `C`/`D`/`E`/`T` line parser and data-block capture per direction (Sink: data flows client→backend; Source: backend→client); ack bytes pass through.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "audit/decode: legacy scp decoder"`

---

### Task 9: ZMODEM decoder/filter (pty channels)

**Files:**
- Create: `internal/audit/decode/zmodem.go`, `zmodem_test.go`

**Interfaces:**
- Consumes: Task 7 contract.
- Produces:
```go
const ZmodemCancel = "\x18\x18\x18\x18\x18\x18\x18\x18\x08\x08\x08\x08\x08\x08\x08\x08\x08\x08" // 8×CAN + 10×BS
func NewZmodem(pol policy.Effective, sink Sink) Decoder
```

- [ ] **Step 1: Write the failing tests** — `TestZmodem_SzFixture` (download: one file, sha256 = sidecar, complete; chunks {1, 3, 4096}); `TestZmodem_RzFixture` (upload); `TestZmodem_PlainTextUntouched` (shell output without ZMODEM ⇒ `Pass == input`, no files); `TestZmodem_DenyDownload` (DenyDownload, feed `zmodem-sz.raw` backend bytes ⇒ the Feed containing `**\x18B00` returns `Reply == ZmodemCancel`, `Pass` ends with `"\r\n[devpod] " + MsgDownloadDenied + "\r\n"`, no `OpenFile`, one `EventPolicy{Rule:"download"}`); `TestZmodem_DenyUpload` (rz's ZRINIT `**\x18B01` from backend, DenyUpload ⇒ same shape with `MsgUploadDenied`); `TestZmodem_MaxFileSize` (ZFILE declared size > max ⇒ cancel + `MsgFileTooLarge`).
- [ ] **Step 2: Run** `go test ./internal/audit/decode/ -run Zmodem -v` — Expected: FAIL.
- [ ] **Step 3: Implement** per the ZMODEM spec (Forsberg `zmodem.txt`; lrzsz is the reference): hex and binary (CRC16 `ZBIN`, CRC32 `ZBIN32`) headers, ZDLE unescaping, ZFILE (`name\0size mtime mode …`), ZDATA subpackets ending ZCRCE/ZCRCG/ZCRCQ/ZCRCW, ZEOF/ZFIN, CAN×5 abort. Bytes outside frames pass through. After a policy cancel, keep parsing and drop frame bytes until the session returns to idle (ZFIN, cancel, or 2 s without frame bytes).
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "audit/decode: ZMODEM decoder with cancel-on-policy"`

---

### Task 10: Store — rows, keys, S3, ClickHouse writer, migrations

**Files:**
- Create: `internal/audit/store/rows.go`, `keys.go`, `keys_test.go`, `s3.go`, `clickhouse.go`, `migrate.go`, `migrations/0001_audit.sql`, `store_integration_test.go` (`//go:build integration`)
- Create: `hack/test-integration.sh`
- Modify: `go.mod` (aws-sdk-go-v2, clickhouse-go/v2, testcontainers-go)

**Interfaces:**
- Produces:
```go
// rows.go — one struct per table in spec §8.2 (audit tables). Every field carries
// `ch:"<column>"` and `json:"<column>"` tags; columns are the snake_case of the field
// (SessionID → session_id, SHA256 → sha256, DevPodUID → devpod_uid). The webui returns
// these rows as JSON unchanged, so the frontend types mirror the snake_case names.
type SessionRow struct { SessionID, User, DevPod, DevPodUID, Owner, GatewayPod, ClientIP, PubkeyFP, AuthKind, ProxyAlias, BackendLocalAddr string; OpenedAt time.Time; ClosedAt *time.Time; CloseReason string; BytesIn, BytesOut int64; PolicySnapshot string; Gaps uint32; Version uint64 }
type ChannelRow struct { SessionID string; ChannelID uint32; Type, Command, Subsystem, Target string; Pty bool; Term string; OpenedAt time.Time; ClosedAt *time.Time; ExitStatus *int32; BytesIn, BytesOut int64; RawRecorded bool; Version uint64 }
type CommandRow struct { EventID string; TS time.Time; SessionID, DevPodUID, User, Source string; ChannelID uint32; PID int32; Cwd, Cmdline string; ExitCode *int32; DurationMS int64; Attribution string; Version uint64 }
type FileEventRow struct { EventID string; TS time.Time; SessionID string; ChannelID uint32; User, DevPodUID, Protocol, Op, Path, Path2 string; Size int64; SHA256 string; Complete, Blocked bool; S3Key string }
type PolicyEventRow struct { TS time.Time; SessionID, User, DevPodUID, Rule, Action, Detail string }
type RawSegmentRow struct { SessionID string; Seq uint32; S3Key string; FirstTS, LastTS time.Time; Bytes, CompressedBytes int64 }
type AlertRow struct { TS time.Time; DevPodUID, SessionID, Kind, Source, Detail string }
type AccessLogRow struct { TS time.Time; Viewer, Action, TargetSession, TargetFile, ClientIP string }
type Batch struct { Sessions []SessionRow; Channels []ChannelRow; Commands []CommandRow; FileEvents []FileEventRow; PolicyEvents []PolicyEventRow; RawSegments []RawSegmentRow; Alerts []AlertRow }
func (b Batch) Empty() bool
// keys.go
func RawKey(opened time.Time, session string, seq uint32) string            // raw/2026/11/09/<s>/000001.seg.zst
func FileKey(opened time.Time, session string, channel uint32, n int) string // files/2026/11/09/<s>/3-1
// s3.go
type ObjectStore interface { Put(ctx context.Context, key string, body io.Reader, size int64) error; Get(ctx context.Context, key string) (io.ReadCloser, error) }
type S3Config struct { Endpoint, Region, Bucket, AccessKey, SecretKey string; PathStyle bool }
func LoadS3Credentials(dir string) (access, secret string, err error)
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) // implements ObjectStore; Put uses manager.Uploader (8 MiB parts)
func (s *S3) EnsureBucket(ctx context.Context) error
func (s *S3) EnsureLifecycle(ctx context.Context, days int) error // expiration rules for prefixes raw/ and files/
// clickhouse.go
type EventStore interface { Insert(ctx context.Context, b Batch, token string) error }
type CHConfig struct { URL, Database, Username, Password string } // URL like clickhouse://host:9000
func LoadCHCredentials(dir string) (user, pass string, err error)
func OpenCH(ctx context.Context, cfg CHConfig) (*CH, error)      // implements EventStore
func (c *CH) Insert(ctx context.Context, b Batch, token string) error // one INSERT per non-empty table, insert_deduplication_token = token+"/"+table
// migrate.go
func Migrate(ctx context.Context, c *CH, retentionDays int) error // embedded migrations/*.sql in order, recorded in schema_migrations; then MODIFY TTL on every audit table
```
- `0001_audit.sql`: database `devpod_audit` (name from `CHConfig.Database`, substituted) with tables per spec §8.2 audit block; engines/order keys as specified; every table `SETTINGS non_replicated_deduplication_window = 10000`; ngrambf_v1 skip index on `commands.cmdline`, bloom_filter on `file_events.path`/`sha256`; plus `schema_migrations(version UInt32, applied_at DateTime)`. TTL is not in CREATE — `Migrate` applies `ALTER TABLE … MODIFY TTL <time col> + INTERVAL N DAY` each run (`opened_at` for sessions/channels, `ts` elsewhere, `first_ts` for raw_segments).

- [ ] **Step 1: Write the failing unit test** `TestKeys` (`RawKey(2026-11-09T23:59Z,"01J…",1) == "raw/2026/11/09/01J…/000001.seg.zst"`; `FileKey(…,3,1) == "files/2026/11/09/01J…/3-1"`; local-time input is converted to UTC).
- [ ] **Step 2: Write the failing integration tests** (testcontainers: ClickHouse module, image from env `CLICKHOUSE_IMAGE`; generic container for Silo, image from env `SILO_IMAGE`, `server /data`): `TestMigrate_Idempotent` (run twice; `system.tables` lists 8 audit tables; second run with 30 days ⇒ `create_table_query` contains `INTERVAL 30 DAY`); `TestInsert_DedupToken` (same batch + token twice ⇒ `count() == 1` per table); `TestS3_PutGetMultipart` (20 MiB Put, Get bytes equal); `TestS3_EnsureBucketAndLifecycleIdempotent` (twice each; `GetBucketLifecycleConfiguration` returns 2 rules with `Expiration.Days == 180`).
- [ ] **Step 3: Create `hack/test-integration.sh`** — `exec go test -tags integration ./internal/audit/... -count=1 "$@"`, defaulting `CLICKHOUSE_IMAGE` and `SILO_IMAGE` to the tags pinned in Task 21's `values.yaml`.
- [ ] **Step 4: Run** `go test ./internal/audit/store/ -v` and `bash hack/test-integration.sh -run 'Migrate|Insert|S3'` — Expected: FAIL.
- [ ] **Step 5: Implement** rows, keys, S3, CH, Migrate.
- [ ] **Step 6: Run** both — Expected: PASS.
- [ ] **Step 7: Commit** `git commit -m "audit/store: S3 + ClickHouse writer and schema migrations"`

---

### Task 11: Store — read queries for the webui

**Files:**
- Create: `internal/audit/store/query.go`, `query_integration_test.go` (`//go:build integration`)

**Interfaces:**
- Consumes: Task 10 `CH`, rows.
- Produces:
```go
var ErrNotFound = errors.New("not found")
type SessionFilter struct { User, DevPodUID string; From, To time.Time; HasFiles, HasAlerts bool; Cursor string; Limit int }
type SessionSummary struct { SessionRow; Commands, Files, PolicyHits, Alerts int }
type SessionDetail struct { Session SessionRow; Channels []ChannelRow; Commands []CommandRow; Files []FileEventRow; Policy []PolicyEventRow; Alerts []AlertRow; Segments []RawSegmentRow }
type CommandFilter struct { User, DevPodUID, Q string; From, To time.Time; Cursor string; Limit int }
type FileFilter struct { User, DevPodUID, Path, SHA256 string; From, To time.Time; Cursor string; Limit int }
type TimeFilter struct { From, To time.Time; Cursor string; Limit int }
func (c *CH) ListSessions(ctx context.Context, f SessionFilter) ([]SessionSummary, string, error) // newest first; next cursor "" at end
func (c *CH) GetSession(ctx context.Context, id string) (*SessionDetail, error)
func (c *CH) SearchCommands(ctx context.Context, f CommandFilter) ([]CommandRow, string, error)
func (c *CH) SearchFiles(ctx context.Context, f FileFilter) ([]FileEventRow, string, error)
func (c *CH) GetFileEvent(ctx context.Context, eventID string) (*FileEventRow, error)
func (c *CH) ListAlerts(ctx context.Context, f TimeFilter) ([]AlertRow, string, error)
func (c *CH) ListAccessLog(ctx context.Context, f TimeFilter) ([]AccessLogRow, string, error)
func (c *CH) InsertAccessLog(ctx context.Context, r AccessLogRow) error
func (c *CH) Presence(ctx context.Context, devpodUIDs []string) (map[string][]string, error) // uid → users with sessions not yet closed
```
- Rules: versioned tables read with `FINAL`; zero `From`/`To` ⇒ error; `Limit` 0 ⇒ 100, capped 1000; cursor = base64(`<unix nanos>|<id>`) of the last row; every query sets `max_execution_time=10`; all values bound as parameters.

- [ ] **Step 1: Write the failing integration tests** with a seeded batch: `TestListSessions_FiltersAndCursor` (user filter; 250 sessions paged by 100 ⇒ 100/100/50, no overlap, last cursor ""); `TestListSessions_FinalUsesLatestVersion` (v1 open + v2 closed ⇒ `ClosedAt` set); `TestListSessions_HasFilesHasAlerts`; `TestGetSession_DetailAndNotFound`; `TestSearchCommands_Substring` (`Q:"rm -rf"`); `TestSearchFiles_BySHA`; `TestPresence_OnlyOpenSessions`; `TestRangeRequired`.
- [ ] **Step 2: Run** `bash hack/test-integration.sh -run 'List|Get|Search|Presence|Range'` — Expected: FAIL.
- [ ] **Step 3: Implement** the queries.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "audit/store: read queries for the webui"`

---

### Task 12: Spool and uploader

**Files:**
- Create: `internal/audit/spool/spool.go`, `uploader.go`, `spool_test.go`, `uploader_test.go`

**Interfaces:**
- Consumes: Task 10 `store.ObjectStore`, `store.EventStore`, `store.Batch`.
- Produces:
```go
type Level int
const ( LevelOK Level = iota; LevelWarn /* ≥80% */; LevelHigh /* ≥90% */; LevelFull /* content budget (max - max/20) exhausted */ )
var ErrFull = errors.New("spool full")
func Open(dir string, maxBytes int64) (*Spool, error)             // creates tmp/, ready/obj/, ready/batch/; usage = existing files (crash recovery)
func (s *Spool) Usage() int64
func (s *Spool) Level() Level
func (s *Spool) Create(objectKey string) (*File, error)          // ErrFull when content budget exhausted
type File struct{ /* io.Writer, io.WriterAt */ }
func (f *File) Seal() error                                       // move to ready/obj/<ulid>.bin + .key sidecar
func (f *File) Discard() error
func (s *Spool) WriteBatch(b store.Batch) error                   // ready/batch/<ulid>.json; may use the reserve; ErrFull only at 100%
type ItemKind uint8
const ( ItemObject ItemKind = iota + 1; ItemBatch )
type Item struct { Kind ItemKind; Path, ObjectKey, Token string; Size int64; Created time.Time } // Token = batch ULID
func (s *Spool) Ready() ([]Item, error)                           // oldest first
func (s *Spool) Done(it Item) error
type Uploader struct {
	Spool *Spool; Objects store.ObjectStore; Events store.EventStore
	Interval time.Duration                 // 1s
	Backoff  func(attempt int) time.Duration // default: 1s·2^n capped 60s
	OnLag    func(seconds float64)           // age of the oldest ready item
}
func (u *Uploader) Run(ctx context.Context)
func (u *Uploader) Flush(ctx context.Context) error // upload everything ready now; first error returned
```

- [ ] **Step 1: Write the failing tests** — `TestSpool_UsageAndRecovery` (write 3 files, reopen ⇒ same Usage); `TestSpool_Levels` (max 100 KiB: 81 KiB ⇒ Warn, 91 KiB ⇒ High); `TestSpool_ContentFullBatchStillAccepted` (fill to 96 % ⇒ `Create` returns `ErrFull`, `WriteBatch` succeeds); `TestUploader_SuccessRemoves` (fakes record Put key/body and Insert token == batch ULID; spool empty after `Flush`); `TestUploader_FailureRetainsAndRetries` (fake fails twice then succeeds; item kept until success; backoff called with attempts 1, 2).
- [ ] **Step 2: Run** `go test ./internal/audit/spool/ -v` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "audit/spool: local spool and uploader"`

---

### Task 13: Recorder and session hooks

**Files:**
- Create: `internal/audit/hooks.go`, `recorder.go`, `session.go`, `channel.go`, `classify.go`, `metrics.go`, `session_test.go`, `recorder_test.go`
- Modify: `go.mod` (`github.com/oklog/ulid/v2`)

**Interfaces:**
- Consumes: Tasks 2–4, 7–12.
- Produces:
```go
// hooks.go
type Dir = decode.Dir
const ( ClientToBackend = decode.ClientToBackend; BackendToClient = decode.BackendToClient )
type Result = decode.Result
type DenyError struct{ Msg string }      // Error() returns Msg; proxy uses it as the reject text
type SessionHooks interface {
	OpenChannel(dir Dir, chType string, extra []byte) (ChannelHooks, error)
	GlobalRequest(dir Dir, typ string, payload []byte) error
	Done() <-chan struct{}               // closed ⇒ proxy tears the connection down
	Close(reason string, bytesIn, bytesOut int64)
}
type ChannelHooks interface {
	Request(dir Dir, typ string, wantReply bool, payload []byte) error // non-nil ⇒ reply false, do not forward
	RequestResult(dir Dir, typ string, ok bool)
	Data(dir Dir, stderr bool, p []byte) Result
	Close()
}
func Noop() SessionHooks
const ( MsgForwardDenied = "blocked by DevPod audit policy: port forwarding disabled"; MsgAgentDenied = "blocked by DevPod audit policy: agent forwarding disabled"; MsgX11Denied = "blocked by DevPod audit policy: X11 forwarding disabled"; MsgAuditUnavailable = "audit storage unavailable (fail-closed policy)" )
// recorder.go
type Config struct {
	SpoolDir string; SpoolMaxBytes int64
	Objects store.ObjectStore; Events store.EventStore
	GatewayPod string
	SegmentMaxBytes int64          // default 8<<20
	SegmentMaxAge, FlushInterval time.Duration // defaults 60s, 5s
	Now func() time.Time
}
type SessionMeta struct { ID, User, DevPod, DevPodUID, Owner, ClientIP, PubkeyFP, AuthKind, ProxyAlias string; OpenedAt time.Time }
func NewSessionID(t time.Time) string
func NewRecorder(cfg Config) (*Recorder, error)
func (r *Recorder) Run(ctx context.Context)         // uploader; per-second segment Tick + batch flush; closes Done() of fail-closed sessions at LevelFull
func (r *Recorder) Drain(ctx context.Context) error // seal + flush everything; at deadline insert AlertRow{Kind:"gap",Source:"gateway"} per session with unuploaded items (10s best-effort)
func (r *Recorder) Start(meta SessionMeta, pol policy.Effective) *Session // *Session implements SessionHooks
func (s *Session) SetBackendAddr(addr string)
// classify.go
func IsOpaqueTransfer(cmd string) bool // argv0 basename rsync (with --server), tar, dd, base64, or cat with a redirection
// metrics.go
func MustRegisterMetrics(reg prometheus.Registerer) // devpod_gateway_audit_{spool_bytes,upload_lag_seconds,gaps_total,decoder_errors_total,policy_actions_total{rule,action}}
```
- Behaviour (spec §4, §7): SessionRow v1 at `Start`, v2 at `Close` (with `PolicySnapshot` = JSON of `Effective`, `Gaps`); ChannelRow v1 at open, v2 at close; exec ⇒ CommandRow `Source:"gateway_exec"`, `EventID: <session>-<channel>`, `Attribution:"exact"`, exit code filled at v2. Decoder choice at the start request: `subsystem sftp` ⇒ SFTP; exec + `ParseSCPCommand` ⇒ SCP (deny `-f` when DenyDownload / `-t` when DenyUpload at the request, rule `download`/`upload`); exec + `IsOpaqueTransfer` ⇒ FileEventRow `Protocol:"opaque", Op:"suspected_transfer", Path:<cmd>`; channel with pty-req ⇒ ZMODEM. `direct-tcpip` deny ⇒ `DenyError{MsgForwardDenied}` rule `forwarding.local`; global `tcpip-forward` deny ⇒ rule `forwarding.remote`; `auth-agent-req@openssh.com` request / `auth-agent@openssh.com` open ⇒ `MsgAgentDenied` rule `agentForwarding`; `x11-req` / `x11` ⇒ `MsgX11Denied` rule `x11Forwarding`. Every policy deny emits a PolicyEventRow; when the decoder's `Event.Path` is known it also emits a FileEventRow with `Blocked:true` and `Op` = the denied direction. Forward channels record data only when `RecordForwardContent`. Decoder panic ⇒ recover, AlertRow `decoder_error`, passthrough; if `FailClosed && HasDeny()` ⇒ `Result{CloseChannel:true}`. Spool `ErrFull` on raw ⇒ count bytes into `Gaps`, one AlertRow `gap` per session. `FailClosed` + `Level() ≥ LevelHigh` ⇒ `OpenChannel` returns `DenyError{MsgAuditUnavailable}`. Injected bytes recorded as `KindInjectTo*`.

- [ ] **Step 1: Write the failing tests** (in-memory fake `ObjectStore`/`EventStore`, temp spool, fixtures driven through `ChannelHooks`):
  - `TestSession_ExecCommandRecorded` — exec `make test`, exit-status 2 ⇒ after `Drain`, CommandRow v2 `Cmdline:"make test"`, `ExitCode:2`.
  - `TestSession_SFTPUploadStoresObjectAndEvent` — FileEventRow sha256 = sidecar; object at `FileKey(...)` with identical bytes; RawSegmentRow(s) present.
  - `TestSession_SCPDownloadDeniedAtRequest` — `Request(exec "scp -f x")` returns `DenyError{MsgDownloadDenied}`; PolicyEventRow rule `download`.
  - `TestSession_DenyLocalForward`, `TestSession_DenyRemoteForward`, `TestSession_DenyAgentAndX11` — errors and rules as above.
  - `TestSession_ForwardContentRecordingToggle` — no DataIn/DataOut records for a direct-tcpip channel by default; present with `RecordForwardContent`.
  - `TestSession_ZmodemOnlyOnPty` — exec channel stdout `"x**\x18B00y"` ⇒ `Pass` identical, no FileEventRow.
  - `TestSession_ConcurrentChannelsRace` — 8 goroutines × open/feed/close (run under `-race`) ⇒ 8 distinct ChannelIDs, 16 ChannelRows (v1+v2).
  - `TestSession_DecoderPanicFailClosedClosesChannel` — package-level `var newSFTP = decode.NewSFTP` swapped for a panicking factory in the test ⇒ `CloseChannel` with FailClosed+DenyDownload, passthrough with FailOpen; AlertRow `decoder_error` either way.
  - `TestRecorder_FailOpenWhenStoresDown` — stores always error: sessions open and Data passes through; spool usage grows; after the content budget is exhausted `Gaps > 0` and an AlertRow `gap` sits in a ready batch; `Done()` never closes.
  - `TestRecorder_FailClosed` — spool at High ⇒ `OpenChannel` returns `DenyError{MsgAuditUnavailable}`; at Full ⇒ `Done()` closes within one Run tick.
  - `TestRecorder_DrainUploadsEverything` — after `Drain`, spool `Ready()` is empty and SessionRow v2 inserted.
- [ ] **Step 2: Run** `go test -race ./internal/audit/ -v` — Expected: FAIL.
- [ ] **Step 3: Implement** the recorder, session, channel state machine, classifier and metrics.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "audit: recorder and session hooks"`

---

### Task 14: Proxy hook points

**Files:**
- Modify: `internal/gateway/proxy.go`, `internal/gateway/proxy_test.go`

**Interfaces:**
- Consumes: Task 13 `audit.SessionHooks`, `audit.ChannelHooks`, `audit.DenyError`, `audit.Noop`.
- Produces: `func Proxy(clientConn ssh.Conn, clientChans <-chan ssh.NewChannel, clientReqs <-chan *ssh.Request, backendConn ssh.Conn, backendChans <-chan ssh.NewChannel, backendReqs <-chan *ssh.Request, stats *ProxyStats, hooks audit.SessionHooks) error` (nil hooks ⇒ `audit.Noop()`).
- Behaviour: channel opens (both directions) go through `hooks.OpenChannel` — error ⇒ `Reject(ssh.Prohibited, err.Error())` without opening the far side; global requests through `GlobalRequest` (error ⇒ `Reply(false)`, not forwarded); channel requests through `Request`, then `RequestResult` with the far side's answer; data pumps become read loops (32 KiB) calling `Data` and writing `Pass` to the destination and `Reply` to the source, each write under that channel side's write mutex; stderr via `Data(BackendToClient, true, …)`; `CloseChannel` closes both channels; `ChannelHooks.Close()` once per channel; `Done()` closes both connections. Existing `startProgSent` EOF ordering and `hostkeys-00@openssh.com` handling stay.

- [ ] **Step 1: Write the failing tests** (reuse `echoBackend`; a scriptable fake `SessionHooks`): `TestProxy_EchoEndToEnd` passes with `nil` hooks (signature updated); `TestProxy_RejectsChannelOpen` (client `OpenChannel` ⇒ `*ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "nope"}`, backend saw no channel); `TestProxy_RequestDenied` (exec ⇒ `ok=false`, backend never receives it); `TestProxy_DataFilterPassAndReply` (hook drops `"secret"` and replies `"NO"` ⇒ client reads `"NO"`, backend echo never returns `"secret"`); `TestProxy_GlobalRequestDenied`; `TestProxy_DoneClosesConnection` (closing the fake's Done ⇒ `Proxy` returns within 1 s).
- [ ] **Step 2: Run** `go test -race ./internal/gateway/ -run Proxy -v` — Expected: FAIL.
- [ ] **Step 3: Implement** the hook points.
- [ ] **Step 4: Run** `go test -race ./internal/gateway/ -v` — Expected: PASS (all existing gateway tests too).
- [ ] **Step 5: Commit** `git commit -m "gateway: audit hook points in the proxy"`

---

### Task 15: Gateway wiring

**Files:**
- Modify: `cmd/gateway/main.go`, `cmd/gateway/main_test.go`, `internal/gateway/audit.go` (log `session_id` as ULID)

**Interfaces:**
- Consumes: Tasks 2, 10, 12, 13, 14.
- Produces (testable helpers in `cmd/gateway`):
```go
func auditEnabled(gc *devpodv1alpha1.GatewayConfig) bool
func sessionPolicy(gc *devpodv1alpha1.GatewayConfig, u *devpodv1alpha1.User) policy.Effective
func bannerText(gc *devpodv1alpha1.GatewayConfig, tmpl *template.Template, meta ssh.ConnMetadata) string // notice line (Spec.Audit.Notice or "本会话将被记录用于安全审计") + "\n" + template output, when auditEnabled
```
- Flags: `--audit-s3-endpoint`, `--audit-s3-bucket` (default `devpod-audit`), `--audit-s3-region` (default `us-east-1`), `--audit-s3-path-style` (default true), `--audit-s3-credentials-dir`, `--audit-clickhouse-url`, `--audit-clickhouse-database` (default `devpod_audit`), `--audit-clickhouse-credentials-dir`, `--audit-spool-dir` (default `/var/spool/devpod-audit`), `--audit-spool-max-bytes` (default `21474836480`), `--shutdown-session-grace` (default `60s`), `--audit-drain-timeout` (default `220s`); gateway pod name from env `POD_NAME`. Recorder built only when `--audit-clickhouse-url` is set.
- Per connection: read `GatewayConfig/default` and the `User` from the informer cache (not the startup copy) for banner, enablement and policy; session ID = `audit.NewSessionID`; after dial `SetBackendAddr(cliConn.LocalAddr().String())`; `hooks.Close(closeReason, in, out)` in the existing deferred block. Shutdown: stop accepting → wait up to session grace → close remaining conns → `recorder.Drain` with the drain timeout.

- [ ] **Step 1: Write the failing tests** — `TestAuditEnabled` (nil audit / Enabled false / true); `TestSessionPolicy_UserOverride` (default Download Deny, user Allow ⇒ allow); `TestBannerText_AuditNotice` (enabled + empty Notice ⇒ first line `本会话将被记录用于安全审计`; custom Notice used; disabled ⇒ exactly the template output; nil template ⇒ notice only).
- [ ] **Step 2: Run** `go test ./cmd/gateway/ -v` — Expected: FAIL.
- [ ] **Step 3: Implement** the helpers and wiring; register `audit.MustRegisterMetrics(metricsReg)`.
- [ ] **Step 4: Run** `go test ./cmd/gateway/ -v && go build ./...` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "gateway: wire the audit recorder, per-session policy and banner notice"`

---

### Task 16: Webui audit wiring and `migrate` subcommand

**Files:**
- Create: `internal/webui/audit.go`, `internal/webui/audit_test.go`
- Modify: `cmd/webui/main.go`, `internal/webui/api_devpods.go` (`Server.Audit` field), `internal/webui/api_users.go` (`features.audit`), `web/src/api.ts` (`Me.features.audit`)

**Interfaces:**
- Consumes: Tasks 10–11.
- Produces:
```go
type AuditStore interface { // satisfied by *store.CH
	ListSessions(context.Context, store.SessionFilter) ([]store.SessionSummary, string, error)
	GetSession(context.Context, string) (*store.SessionDetail, error)
	SearchCommands(context.Context, store.CommandFilter) ([]store.CommandRow, string, error)
	SearchFiles(context.Context, store.FileFilter) ([]store.FileEventRow, string, error)
	GetFileEvent(context.Context, string) (*store.FileEventRow, error)
	ListAlerts(context.Context, store.TimeFilter) ([]store.AlertRow, string, error)
	ListAccessLog(context.Context, store.TimeFilter) ([]store.AccessLogRow, string, error)
	InsertAccessLog(context.Context, store.AccessLogRow) error
	Presence(context.Context, []string) (map[string][]string, error)
}
type AuditDeps struct { Store AuditStore; Objects store.ObjectStore }
// Server gains: Audit *AuditDeps   (nil ⇒ audit features off)
func (s *Server) freshSession(w http.ResponseWriter, r *http.Request) (Session, bool) // sessionFrom + Admin = s.adminFor(user, cached User)
```
- `cmd/webui` flags: `--audit-clickhouse-url`, `--audit-clickhouse-database` (default `devpod_audit`), `--audit-clickhouse-credentials-dir`, `--audit-s3-endpoint`, `--audit-s3-bucket`, `--audit-s3-region`, `--audit-s3-path-style`, `--audit-s3-credentials-dir`. Subcommand `devpod-webui migrate` with the same storage flags plus `--retention-days` (default 180), `--manage-lifecycle` (default false), `--create-bucket` (default false): runs `EnsureBucket` (if set), `Migrate`, `EnsureLifecycle` (if set); retries the ClickHouse connection for up to 5 minutes (bundled ClickHouse may still be starting).

- [ ] **Step 1: Write the failing envtest tests** — `TestFreshSession_RevokedAdmin` (cookie minted admin=true; User `spec.admin=false`, not in `Admins` ⇒ `freshSession` returns `Admin=false`); `TestFreshSession_AllowlistAdmin`; `TestMe_FeatureAudit` (`Audit` nil ⇒ `features.audit=false`; set ⇒ true).
- [ ] **Step 2: Run** `bash hack/test.sh -run 'FreshSession|FeatureAudit' ./internal/webui/` — Expected: FAIL.
- [ ] **Step 3: Implement**; `go build ./cmd/webui && ./devpod-webui migrate -h` shows the migrate flags.
- [ ] **Step 4: Run** the tests — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "webui: audit backend wiring, fresh-admin check, migrate subcommand"`

---

### Task 17: Webui audit API

**Files:**
- Create: `internal/webui/api_audit.go`, `internal/webui/api_audit_test.go`
- Modify: `internal/webui/server.go` (routes)

**Interfaces:**
- Consumes: Task 16 `AuditDeps`, `freshSession`; Task 5 `rawlog.WriteCast`, `rawlog.Concat`, `rawlog.OpenSegment`; Task 2 `policy.Resolve`/`Warnings`.
- Produces (all JSON unless noted; `Audit == nil` ⇒ 404 `AUDIT_DISABLED`):

| Route | Rule |
|---|---|
| `GET /api/audit/sessions` | query `user, devpod, from, to (RFC3339), hasFiles, hasAlerts, cursor, limit`; non-admin ⇒ `User` forced to self; `devpod` name → UID via the cached DevPod. Body `{items, next}` |
| `GET /api/audit/sessions/{id}` | non-admin and `Session.User != self` ⇒ 404; admin viewing another user ⇒ access log `view_session` |
| `GET /api/audit/sessions/{id}/channels/{ch}/cast` | same visibility; `Content-Type: application/x-asciicast`; streams `WriteCast` over `Segments`; access log `replay` |
| `GET /api/audit/sessions/{id}/channels/{ch}/raw?stream=in\|out\|err` | admin only (403 `FORBIDDEN`); `application/octet-stream`; access log `download_raw` |
| `GET /api/audit/files/{event}/content` | visibility via the event's session user; `Content-Disposition: attachment; filename="<base>"`; access log `download_file` |
| `GET /api/audit/commands`, `GET /api/audit/files` | non-admin forced to self |
| `GET /api/audit/alerts`, `GET /api/audit/access-log` | fresh admin only |
| `GET /api/audit/presence[?scope=all]` | non-admin: own DevPods; admin with `scope=all`: every DevPod. Body `{items: {"<devpod>": {"count": n, "users": [...]}}}` |
| `GET /api/admin/users/{name}/audit-policy` | fresh admin; `{default, user, effective, warnings}` |
| `PUT /api/admin/users/{name}/audit-policy` | fresh admin; body `AuditPolicy` or `null`; updates `User.spec.auditPolicy`; 404 when no User CR; access log `edit_policy` |
| `GET /api/admin/audit/policy-default` | fresh admin; `GatewayConfig/default.spec.audit.policy` |

- [ ] **Step 1: Write the failing envtest tests** (fake `AuditStore` that records filters; fake `ObjectStore` serving a segment built with `rawlog`): `TestAuditSessions_NonAdminForcedToSelf` (query `user=bob` as alice ⇒ store saw `User:"alice"`); `TestAuditSessions_DefaultRange7Days`; `TestAuditSessionDetail_OtherUser404`; `TestAuditSessionDetail_AdminLogsView`; `TestAuditCast_StreamsAndLogs` (body first line is the cast header; access log `replay`); `TestAuditRaw_AdminOnly`; `TestAuditFileContent_Disposition`; `TestAuditPresence_OwnerSeesUsers`; `TestAuditPolicy_PutUpdatesUserAndLogs`; `TestAuditPolicy_NoUserCR404`; `TestAuditAlerts_RevokedAdminForbidden` (cookie admin, fresh not admin ⇒ 403); `TestAuditDisabled_404`.
- [ ] **Step 2: Run** `bash hack/test.sh ./internal/webui/ -run Audit` — Expected: FAIL.
- [ ] **Step 3: Implement** the handlers and routes.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "webui: audit API"`

---

### Task 18: Frontend — table-style DevPod list

**Files:**
- Create: `web/src/format.ts`, `web/src/format.test.ts`, `web/src/columns.ts`, `web/src/columns.test.ts`, `web/src/components/DevPodTable.tsx`
- Modify: `web/src/api.ts` (presence), `web/src/pages/PodList.tsx`, `web/src/pages/AdminDevPods.tsx`

**Interfaces:**
- Consumes: Task 17 `GET /api/audit/presence`.
- Produces:
```ts
export function fmtBytes(n: number): string          // 1536 → "1.5K", 58.1e9 → "58.1G" (1024-based, 1 decimal, units K M G T)
export function fmtDuration(ms: number): string       // 7920000 → "2h12m", 194400000 → "2d6h", 42000 → "42s"
export function fmtAge(iso: string, now: Date): string // fmtDuration(now - iso)
export function failureRetryIn(retryCount: number, lastFailureAt: string, now: Date): number // seconds; backoff 10s·2^(n-1) capped 300s (controller's failureBackoff)
export type ColumnKey = "name"|"phase"|"cpu"|"memory"|"gpu"|"net"|"rdma"|"sess"|"up"|"image"|"ssh"|"actions"
export const HIDE_ORDER: ColumnKey[] = ["image", "rdma", "net", "up"]
export function visibleColumns(width: number, available: ColumnKey[]): ColumnKey[] // ≥1100 all; <1100 −image; <980 −rdma; <860 −net; <760 −up
export function DevPodTable(props: { items: DevPod[]; me: Me; presence?: Record<string, {count: number; users: string[]}>; showOwner?: boolean; onToggle: (name: string, running: boolean) => void }): JSX.Element
```
- Phase-1 columns (spec §9.4, metrics arrive in phase 2): Name (suffix; Owner column when `showOwner`), Phase, CPU = summed container cpu limits, Memory = summed memory limits, GPU = summed `nvidia.com/gpu` limits, Sess = presence count + users, **Age** (phase 2 renames to Up), Image = first container image's last path segment, SSH copy icon, Hibernate/Wake. Row ≈ 30 px, mono 12 px, tokens from `index.css`. Non-running rows merge the metric cells into one status cell: Pending ⇒ `status.message` or `starting…`; Stopped ⇒ `hibernated <fmtAge(hibernatedAt)> ago` + `· home PVC <size> kept` when persistence; Failed ⇒ `status.message · retry #<n> in <failureRetryIn>s`. Toolbar: name/image filter, Phase chips with counts, totals vs quota from `me.usage`/`me.quota`. Presence polled every 10 s when `features.audit`; without audit the `sess` column is not in `available`. `net` and `rdma` are not in `available` until phase 2.

- [ ] **Step 1: Write the failing Vitest tests** — `format.test.ts` (the examples above; `failureRetryIn(3, t-30s, t) === 10`); `columns.test.ts` (`visibleColumns(1200, all)` has image; `1000` drops image only; `800` drops image, rdma, net; `700` also up; keys absent from `available` never appear).
- [ ] **Step 2: Run** `cd web && npm test` — Expected: FAIL.
- [ ] **Step 3: Implement** helpers, `DevPodTable`; switch `PodList` and `AdminDevPods` (with `showOwner`, `scope=all` presence) to it.
- [ ] **Step 4: Run** `cd web && npm test && npm run build` — Expected: PASS, build OK.
- [ ] **Step 5: Commit** `git commit -m "webui: table-style DevPod list"`

---

### Task 19: Frontend — tabbed DevPod detail page

**Files:**
- Modify: `internal/webui/api_devpods.go`, `internal/webui/watch_stream.go` (+ tests), `web/src/api.ts`, `web/src/pages/PodDetail.tsx`
- Create: `web/src/tabs.ts`, `web/src/tabs.test.ts`

**Interfaces:**
- Produces: `GET /api/devpods/{name}` and the SSE `devpod` message gain `pod?: {node: string; ip: string; startTime?: string}` read from the cached Pod; `export function tabFromHash(hash: string, tabs: string[]): string` (unknown / empty ⇒ first tab).
- Layout (spec §9.4, phase 1 subset): header (name, phase, SSH copy, Hibernate, Delete); meta row (owner, node, pod IP, image, up from `pod.startTime`, shell, home volume, Kore binding summary, collaborators); tabs via URL hash — `Overview` (existing Access card, SSH config, Kore node layout), `Sessions` (only when `features.audit`: rows of `/api/audit/sessions?devpod=<name>` with user, from, start, duration, commands, files, blocked/alert tags, ▶ replay link to `/audit/sessions/<id>`), `Events` (existing live list). KPI cards and Monitoring/Processes/Container tabs are phase 2.

- [ ] **Step 1: Write the failing tests** — Go envtest `TestGetDevPod_IncludesPodInfo` (Pod with `spec.nodeName`, `status.podIP`, `status.startTime` ⇒ JSON `pod` fields); Vitest `tabs.test.ts` (`tabFromHash("#sessions", ["overview","sessions","events"]) === "sessions"`, `"#bogus"` ⇒ `"overview"`).
- [ ] **Step 2: Run** `bash hack/test.sh ./internal/webui/ -run PodInfo` and `cd web && npm test` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** both plus `npm run build` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "webui: tabbed DevPod detail page with sessions tab"`

---

### Task 20: Frontend — audit pages, player, policy editor

**Files:**
- Create: `web/src/audit.ts`, `web/src/audit.test.ts`, `web/src/components/CastPlayer.tsx`, `web/src/components/AuditPolicyEditor.tsx`, `web/src/policy.test.ts`, `web/src/pages/audit/{Sessions,SessionDetail,Commands,Files,Alerts,AccessLog}.tsx`
- Modify: `web/package.json` (`asciinema-player@^3`), `web/src/api.ts`, `web/src/main.tsx` (routes), `web/src/ui.tsx` (`Shell` nav), `web/src/pages/AdminUsers.tsx`

**Interfaces:**
- Consumes: Task 17 endpoints.
- Produces:
```ts
export type TimelineItem = { ts: string; kind: "channel"|"command"|"file"|"policy"|"alert"; channelId?: number; text: string; ref?: unknown }
export function mergeTimeline(d: SessionDetail): TimelineItem[]            // all items, ascending by ts, stable
export function castOffset(channelOpenedAt: string, ts: string): number    // seconds ≥ 0
export function formFromPolicy(p: AuditPolicy | null): PolicyForm           // tri-state per field: "inherit"|"Allow"|"Deny" (FailMode "inherit"|"Open"|"Closed"), maxFileSize string, recordContent "inherit"|"true"|"false"
export function policyFromForm(f: PolicyForm): AuditPolicy | null           // all inherit ⇒ null
export function CastPlayer(props: { src: string; seekTo?: number }): JSX.Element // wraps asciinema-player; changing seekTo calls player.seek
```
- Routes: `/audit/sessions`, `/audit/sessions/:id`, `/audit/commands`, `/audit/files`, `/audit/alerts`, `/audit/access-log`. `Shell` shows an "Audit" nav group when `features.audit` (alerts and access log only for admins). Session detail = header (user, DevPod, client IP, start, duration, bytes, policy snapshot) + channel selector + `CastPlayer` (`/api/audit/sessions/<id>/channels/<ch>/cast`) + timeline; clicking a timeline item sets `seekTo = castOffset(channel.openedAt, item.ts)`; file items have a download link with confirm text `此操作将被记录` and show `blocked` / `incomplete` tags. AdminUsers gains a per-user "Audit policy" panel using `AuditPolicyEditor`, showing effective values and warnings from the GET endpoint.

- [ ] **Step 1: Write the failing Vitest tests** — `audit.test.ts` (`mergeTimeline` orders mixed kinds by ts and keeps input order for equal ts; `castOffset("…:00Z","…:02.5Z") === 2.5`; negative clamps to 0); `policy.test.ts` (`policyFromForm(formFromPolicy(p))` round-trips a full policy; all-inherit ⇒ `null`; `maxFileSize: ""` ⇒ field omitted).
- [ ] **Step 2: Run** `cd web && npm test` — Expected: FAIL.
- [ ] **Step 3: Implement** helpers, components, pages, routes, nav.
- [ ] **Step 4: Run** `cd web && npm test && npm run build` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "webui: audit pages, session replay, audit policy editor"`

---

### Task 21: Chart — audit wiring and bundled storage

**Files:**
- Create: `deploy/chart/templates/_audit.tpl`, `audit-s3.yaml`, `audit-clickhouse.yaml`, `audit-migrate.yaml`, `hack/test-chart.sh`
- Modify: `deploy/chart/values.yaml`, `templates/gateway.yaml`, `templates/webui.yaml`, `templates/webui-rbac.yaml` (ClusterRole: `gatewayconfigs` get/list/watch, needed by the policy endpoints), `templates/gatewayconfig.yaml`, `.github/workflows/ci.yaml` (helm-render job runs `bash hack/test-chart.sh`), `docs/superpowers/specs/2026-11-09-devpod-audit-telemetry-design.md` (§3.2: bucket + lifecycle are created by the migrate Job via aws-sdk instead of `mcli`; §10.2: no preStop hook — the gateway drains on SIGTERM itself)

**Interfaces:**
- values (new block):
```yaml
audit:
  enabled: false
  notice: ""
  retentionDays: 180
  policy: {}                    # → GatewayConfig.spec.audit.policy
  spool: {sizeLimit: 20Gi}
  s3:
    endpoint: ""                # external mode
    region: us-east-1
    bucket: devpod-audit
    pathStyle: true
    credentialsSecret: ""       # external: keys access-key, secret-key
    manageLifecycle: false      # bundled mode always manages
    bundled: {enabled: false, image: {repository: docker.io/pgsty/silo, tag: "<pin>"}, storage: 100Gi, storageClass: ""}
  clickhouse:
    url: ""                     # external, e.g. clickhouse://host:9000
    database: devpod_audit
    credentialsSecrets: {gateway: "", webui: "", migrate: ""}  # external: keys username, password
    bundled: {enabled: false, image: {repository: clickhouse/clickhouse-server, tag: "<pin newest LTS>"}, storage: 50Gi, storageClass: ""}
```
- Rendered objects when `audit.enabled`: GatewayConfig `spec.audit {enabled: true, notice, policy}`; gateway Deployment gains `POD_NAME` env, audit flags (Task 15), `emptyDir {sizeLimit: .Values.audit.spool.sizeLimit}` at `/var/spool/devpod-audit` with `--audit-spool-max-bytes` equal to it in bytes, credential mounts, `terminationGracePeriodSeconds: 300`; webui Deployment gains audit flags + mounts; Helm hook Job `devpod-audit-migrate` (`post-install,post-upgrade`, `before-hook-creation,hook-succeeded`, `backoffLimit: 6`) running `devpod-webui migrate --retention-days … --manage-lifecycle=<bundled or s3.manageLifecycle> --create-bucket=<bundled>`.
- Bundled S3: Secret `devpod-audit-s3` (`access-key`, `secret-key`, generated once via `lookup`), Silo StatefulSet `devpod-audit-s3` (1 replica, PVC, args `server /data`, root user/password env from the Secret), Service `:9000`.
- Bundled ClickHouse: StatefulSet `devpod-audit-clickhouse` (1 replica, PVC), Service `:9000`/`:8123`, ConfigMap `users.d/devpod.xml` defining `devpod_gateway` (`GRANT INSERT ON devpod_audit.*`), `devpod_webui` (`GRANT SELECT, INSERT ON devpod_audit.*`), `devpod_migrate` (`GRANT ALL ON devpod_audit.*`, `CREATE DATABASE`) with `<grants><query>`, passwords from Secrets `devpod-audit-ch-{gateway,webui,migrate}` (`username`, `password`; generated via `lookup`).
- NetworkPolicies on both bundled StatefulSets: ingress only from pods labeled `app.kubernetes.io/name` ∈ {`devpod-gateway`, `devpod-webui`, `devpod-audit-migrate`}.
- Pin both image tags to concrete versions at implementation time (Docker Hub), and use the same tags as defaults in `hack/test-integration.sh`.

- [ ] **Step 1: Write `hack/test-chart.sh`** (fails first): renders three value sets and asserts with `grep`/`yq`-free checks — (a) defaults: no object named `devpod-audit-*`, gateway args contain no `--audit-`; (b) `--set audit.enabled=true,audit.s3.bundled.enabled=true,audit.clickhouse.bundled.enabled=true`: StatefulSets `devpod-audit-s3` and `devpod-audit-clickhouse`, Job `devpod-audit-migrate` with `--create-bucket=true`, gateway arg `--audit-clickhouse-url=clickhouse://devpod-audit-clickhouse.devpod-system.svc:9000`, `terminationGracePeriodSeconds: 300`, GatewayConfig contains `audit:`; (c) external (`audit.s3.endpoint`, `audit.clickhouse.url`, secret names set): no StatefulSets, gateway mounts the given secret names; `helm lint` passes for all three.
- [ ] **Step 2: Run** `bash hack/test-chart.sh` — Expected: FAIL.
- [ ] **Step 3: Implement** templates and values; update the spec §3.2 sentence.
- [ ] **Step 4: Run** `bash hack/test-chart.sh` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "chart: audit wiring with bundled Silo and ClickHouse"`

---

### Task 22: End-to-end on kind

**Files:**
- Create: `hack/e2e-audit.sh`, `test/e2e/images/audit-box/Dockerfile` (`FROM ubuntu:24.04` + `lrzsz`)
- Modify: `QUICKSTART.md` (new section "6c. (Optional) Enable session audit"), `README.md` (commands list)

**Interfaces:**
- Consumes: everything above; runs after `bash hack/e2e-up.sh` (and `hack/e2e-webui.sh` for the webui part).
- Host tools: `ssh`, `sftp`, `scp`, `expect`, `lrzsz`.

- [ ] **Step 1: Write `hack/e2e-audit.sh`**: build/load the audit-box image; `helm upgrade --reuse-values --set audit.enabled=true,audit.s3.bundled.enabled=true,audit.clickhouse.bundled.enabled=true`; wait for the migrate Job; create User `alice` (test key) + DevPod `alice-audit` on audit-box; port-forward the gateway; then:
  1. `ssh alice+audit@… 'echo hi; exit 3'` → expect exit 3;
  2. `sftp` put + get a 1 MiB file; `scp -O` put; `expect` script running `sz` inside an interactive session with local `rz`;
  3. set `User.spec.auditPolicy.download=Deny`; `sftp get` must fail with `blocked by DevPod audit policy: downloads disabled`; reset;
  4. after 10 s, `kubectl exec` into `devpod-audit-clickhouse-0` and assert with `clickhouse-client`: ≥1 `commands` row `cmdline='echo hi; exit 3'` with `exit_code=3`; `file_events` rows for protocols `sftp`, `scp`, `zmodem` with non-empty `sha256` and `complete=1`; ≥1 `policy_events` row `rule='download'`;
  5. via the webui API (fake-IdP login from `e2e-webui.sh`): `GET /api/audit/sessions` returns alice's sessions; the cast endpoint returns a body whose first line contains `"version":2`; a second user gets 404 on alice's session.
- [ ] **Step 2: Run** `bash hack/e2e-up.sh && bash hack/e2e-webui.sh && bash hack/e2e-audit.sh` — Expected: script ends with `e2e-audit: OK`.
- [ ] **Step 3: Write the QUICKSTART section** (bundled one-liner, external mode secrets, policy example, retention) and README command line.
- [ ] **Step 4: Commit** `git commit -m "e2e: session audit end-to-end; docs"`

---

### Task 23: Phase-1 measurements (spec §14 items 1–3)

**Files:**
- Create: `docs/superpowers/notes/2026-11-audit-phase1-measurements.md`
- Modify: spec §8.6 / §14 with the findings

- [ ] **Step 1: Gateway throughput** on the kind cluster, audit off vs on (bundled storage): `scp -O` of a 1 GiB file through the gateway (MB/s), and `ssh -t … 'yes | head -c 500000000'` wall time. Record both pairs and gateway CPU (`kubectl top pod`).
- [ ] **Step 2: Silo compatibility** — record the pinned Silo tag and that `hack/test-integration.sh` (multipart Put, lifecycle) passed against it.
- [ ] **Step 3: VS Code Remote-SSH path (needs the human partner)** — connect VS Code through the gateway with default settings, open a folder, then query `channels` for that session: report whether the bulk bytes sit on `exec` channels or `direct-tcpip` channels, and the bytes per hour. If exec carries the bulk, file a follow-up to classify the VS Code exec server as an opaque channel governed by `forwarding.recordContent` (spec §8.6).
- [ ] **Step 4: Commit** `git commit -m "docs: audit phase-1 measurements"`
