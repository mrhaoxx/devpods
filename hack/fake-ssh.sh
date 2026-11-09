#!/usr/bin/env bash
# Stand-in for ssh(1) used by `scp -O -S hack/fake-ssh.sh` in
# hack/gen-audit-fixtures.sh: ignores ssh options and the host, and runs
# the remote command (scp's last argument) locally under audit-fixture so
# the legacy scp protocol is recorded. AUDIT_FIXTURE_OUT names the output.
set -euo pipefail
cmd="${!#}"
exec "${AUDIT_FIXTURE_BIN:?}" -o "${AUDIT_FIXTURE_OUT:?}" -request "exec:${cmd}" -- sh -c "${cmd}"
