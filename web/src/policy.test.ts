import { describe, expect, it } from "vitest";
import { formFromPolicy, policyFromForm } from "./policy";
import { AuditPolicy } from "./api";

describe("policy form", () => {
  it("round-trips a full policy", () => {
    const p: AuditPolicy = {
      download: "Deny",
      upload: "Allow",
      maxFileSize: "100Mi",
      forwarding: { local: "Allow", remote: "Deny", recordContent: true },
      agentForwarding: "Deny",
      x11Forwarding: "Allow",
      failMode: "Closed",
    };
    expect(policyFromForm(formFromPolicy(p))).toEqual(p);
  });
  it("all-inherit is null", () => {
    expect(policyFromForm(formFromPolicy(null))).toBeNull();
    expect(policyFromForm(formFromPolicy({}))).toBeNull();
  });
  it("omits an empty maxFileSize and unset forwarding fields", () => {
    const f = formFromPolicy({ download: "Deny" });
    f.maxFileSize = "";
    expect(policyFromForm(f)).toEqual({ download: "Deny" });
    f.forwardingLocal = "Deny";
    expect(policyFromForm(f)).toEqual({ download: "Deny", forwarding: { local: "Deny" } });
  });
});
