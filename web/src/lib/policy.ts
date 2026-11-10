import { AuditPolicy, PolicyAction } from "../data/api";

// Editing model of a per-user audit policy: every field can inherit the
// cluster default.
export type Tri = "inherit" | PolicyAction;
export type PolicyForm = {
  download: Tri;
  upload: Tri;
  maxFileSize: string;
  forwardingLocal: Tri;
  forwardingRemote: Tri;
  recordContent: "inherit" | "true" | "false";
  agentForwarding: Tri;
  x11Forwarding: Tri;
  failMode: "inherit" | "Open" | "Closed";
};

export function formFromPolicy(p: AuditPolicy | null): PolicyForm {
  const rc = p?.forwarding?.recordContent;
  return {
    download: p?.download ?? "inherit",
    upload: p?.upload ?? "inherit",
    maxFileSize: p?.maxFileSize ?? "",
    forwardingLocal: p?.forwarding?.local ?? "inherit",
    forwardingRemote: p?.forwarding?.remote ?? "inherit",
    recordContent: rc === undefined ? "inherit" : rc ? "true" : "false",
    agentForwarding: p?.agentForwarding ?? "inherit",
    x11Forwarding: p?.x11Forwarding ?? "inherit",
    failMode: p?.failMode ?? "inherit",
  };
}

// policyFromForm drops inherited fields; nothing set → null.
export function policyFromForm(f: PolicyForm): AuditPolicy | null {
  const p: AuditPolicy = {};
  if (f.download !== "inherit") p.download = f.download;
  if (f.upload !== "inherit") p.upload = f.upload;
  if (f.maxFileSize.trim()) p.maxFileSize = f.maxFileSize.trim();
  const fw: NonNullable<AuditPolicy["forwarding"]> = {};
  if (f.forwardingLocal !== "inherit") fw.local = f.forwardingLocal;
  if (f.forwardingRemote !== "inherit") fw.remote = f.forwardingRemote;
  if (f.recordContent !== "inherit") fw.recordContent = f.recordContent === "true";
  if (Object.keys(fw).length) p.forwarding = fw;
  if (f.agentForwarding !== "inherit") p.agentForwarding = f.agentForwarding;
  if (f.x11Forwarding !== "inherit") p.x11Forwarding = f.x11Forwarding;
  if (f.failMode !== "inherit") p.failMode = f.failMode;
  return Object.keys(p).length ? p : null;
}
