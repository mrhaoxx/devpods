#!/usr/bin/env bash
# Regenerate the protocol fixtures in internal/audit/decode/testdata by
# recording REAL clients (OpenSSH sftp / scp -O, lrzsz sz/rz) talking to
# REAL servers through hack/audit-fixture. CI does not run this; the
# generated .raw/.json files are committed.
#
# Requires: openssh client + sftp-server, lrzsz, socat, openssl.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/internal/audit/decode/testdata"
mkdir -p "$ROOT/bin" "$OUT"
go build -o "$ROOT/bin/audit-fixture" "$ROOT/hack/audit-fixture"
export AUDIT_FIXTURE_BIN="$ROOT/bin/audit-fixture"
FAKE_SSH="$ROOT/hack/fake-ssh.sh"

SFTP_SERVER=""
for p in /usr/lib/openssh/sftp-server /usr/libexec/openssh/sftp-server /usr/libexec/sftp-server; do
  if [ -x "$p" ]; then SFTP_SERVER="$p"; break; fi
done
[ -n "$SFTP_SERVER" ] || { echo "sftp-server not found" >&2; exit 1; }

sha() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
size() { wc -c <"$1" | tr -d ' '; }
# Deterministic pseudo-random content: AES-CTR keystream over zeros.
# (openssl dies of SIGPIPE when head stops reading; mask that under pipefail.)
gen() { (openssl enc -aes-128-ctr -nosalt -K "$(printf '%032d' "$2")" -iv 00000000000000000000000000000000 -in /dev/zero 2>/dev/null || true) | head -c "$1"; }
entry() { printf '{"direction":"%s","path":"%s","size":%s,"sha256":"%s"}' "$1" "$2" "$(size "$3")" "$(sha "$3")"; }
sidecar() { local name=$1; shift; local IFS=,; printf '{"files":[%s]}\n' "$*" >"$OUT/$name.json"; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"

# --- SFTP: mkdir, put 1 MiB, get it back, rename, rm ----------------------
gen 1048576 1 >up.bin
cat >batch <<'EOF'
mkdir d
put up.bin d/remote.bin
get d/remote.bin down.bin
rename d/remote.bin d/renamed.bin
rm d/renamed.bin
rmdir d
EOF
sftp -q -b batch -D "$AUDIT_FIXTURE_BIN -o $OUT/sftp-put-get.raw -request subsystem:sftp -- $SFTP_SERVER" >/dev/null
sidecar sftp-put-get "$(entry upload d/remote.bin up.bin)" "$(entry download d/remote.bin down.bin)"

# --- SFTP pipelined: 4 KiB requests, 64 outstanding -----------------------
gen 2097152 2 >pipe.bin
printf 'put pipe.bin p.bin\nget p.bin pipe-back.bin\nrm p.bin\n' >batch2
sftp -q -B 4096 -R 64 -b batch2 -D "$AUDIT_FIXTURE_BIN -o $OUT/sftp-pipelined.raw -request subsystem:sftp -- $SFTP_SERVER" >/dev/null
cmp pipe.bin pipe-back.bin
sidecar sftp-pipelined "$(entry upload p.bin pipe.bin)" "$(entry download p.bin pipe-back.bin)"

# --- legacy scp (-O) ------------------------------------------------------
gen 1048576 3 >scp.bin
mkdir -p scpdst
AUDIT_FIXTURE_OUT="$OUT/scp-upload.raw" scp -q -O -S "$FAKE_SSH" scp.bin "x:$WORK/scpdst/"
sidecar scp-upload "$(entry upload scp.bin scp.bin)"

AUDIT_FIXTURE_OUT="$OUT/scp-download.raw" scp -q -O -S "$FAKE_SSH" "x:$WORK/scp.bin" scp-back.bin
cmp scp.bin scp-back.bin
sidecar scp-download "$(entry download scp.bin scp.bin)"

mkdir -p tree/sub && gen 4096 4 >tree/a.bin && gen 8192 5 >tree/sub/b.bin && gen 100 6 >tree/sub/c.bin
mkdir -p rdst
AUDIT_FIXTURE_OUT="$OUT/scp-recursive.raw" scp -q -O -r -S "$FAKE_SSH" tree "x:$WORK/rdst/"
sidecar scp-recursive "$(entry upload tree/a.bin tree/a.bin)" "$(entry upload tree/sub/b.bin tree/sub/b.bin)" "$(entry upload tree/sub/c.bin tree/sub/c.bin)"

# --- ZMODEM: backend sz (download) and backend rz (upload) ---------------
gen 1048576 7 >zm.bin
mkdir -p zrecv && (cd zrecv && socat EXEC:"$AUDIT_FIXTURE_BIN -o $OUT/zmodem-sz.raw -pty xterm -request shell -- sz -q $WORK/zm.bin" EXEC:"rz -q -y")
cmp zm.bin zrecv/zm.bin
sidecar zmodem-sz "$(entry download zm.bin zm.bin)"

mkdir -p zrecv2 && (cd zrecv2 && socat EXEC:"$AUDIT_FIXTURE_BIN -o $OUT/zmodem-rz.raw -pty xterm -request shell -- rz -q -y" EXEC:"sz -q $WORK/zm.bin")
cmp zm.bin zrecv2/zm.bin
sidecar zmodem-rz "$(entry upload zm.bin zm.bin)"

ls -la "$OUT"
