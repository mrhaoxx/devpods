import { describe, expect, it } from "vitest";
import type { DevPod } from "../../data/api";
import { devpodToConfig } from "./exportConfig";
import { parseVars } from "./vars";

const dp = (over: Record<string, unknown> = {}): DevPod =>
  ({
    apiVersion: "devpod.io/v1alpha1",
    kind: "DevPod",
    metadata: { name: "alice-sg", uid: "u-1", creationTimestamp: "2026-11-11T00:00:00Z", annotations: { "devpod.io/http-id": "abc" }, resourceVersion: "7" },
    spec: {
      owner: "alice",
      running: true,
      shell: "zsh",
      pod: {
        spec: {
          hostNetwork: true,
          nodeSelector: { "kubernetes.io/hostname": "m602" },
          containers: [
            {
              name: "dev",
              image: "10.12.11.7:16622/sglang",
              command: ["sleep", "infinity"],
              resources: { limits: { "nvidia.com/gpu": "8" } },
              securityContext: { privileged: true, capabilities: { add: ["IPC_LOCK"] } },
              volumeMounts: [{ mountPath: "/models", name: "model", readOnly: true }],
            },
          ],
          volumes: [{ name: "dshm", emptyDir: { medium: "Memory", sizeLimit: "1300Gi" } }],
        },
      },
    },
    status: { phase: "Running", endpoint: "10.0.0.1:2222" },
    ...over,
  }) as unknown as DevPod;

describe("a DevPod saved as a config", () => {
  it("is the manifest with its name, owner and node as variables", () => {
    expect(devpodToConfig(dp())).toBe(
      [
        "apiVersion: devpod.io/v1alpha1",
        "kind: DevPod",
        "metadata:",
        "  name: ${NAME}",
        "spec:",
        "  owner: ${OWNER}",
        "  running: true",
        "  shell: zsh",
        "  pod:",
        "    spec:",
        "      hostNetwork: true",
        "      nodeSelector:",
        "        kubernetes.io/hostname: ${NODE}",
        "      containers:",
        "        - name: dev",
        "          image: 10.12.11.7:16622/sglang",
        "          command:",
        "            - sleep",
        "            - infinity",
        "          resources:",
        "            limits:",
        '              nvidia.com/gpu: "8"',
        "          securityContext:",
        "            privileged: true",
        "            capabilities:",
        "              add:",
        "                - IPC_LOCK",
        "          volumeMounts:",
        "            - mountPath: /models",
        "              name: model",
        "              readOnly: true",
        "      volumes:",
        "        - name: dshm",
        "          emptyDir:",
        "            medium: Memory",
        "            sizeLimit: 1300Gi",
        "",
      ].join("\n"),
    );
  });

  it("has exactly those variables", () => {
    expect(parseVars(devpodToConfig(dp())).map((v) => v.name)).toEqual(["NAME", "OWNER", "NODE"]);
  });

  it("without a pinned node there is no ${NODE}", () => {
    const d = dp();
    delete d.spec.pod!.spec!.nodeSelector;
    const text = devpodToConfig(d);
    expect(text).not.toContain("${NODE}");
    expect(text).not.toContain("nodeSelector");
  });

  it("text that looks like a variable stays text", () => {
    const d = dp();
    (d.spec.pod!.spec!.containers![0] as unknown as { command: string[] }).command = ["sh", "-c", "echo ${HOME} $${X}\nexit 0\n"];
    const text = devpodToConfig(d);
    expect(parseVars(text).map((v) => v.name)).toEqual(["NAME", "OWNER", "NODE"]);
    expect(text).toContain("echo $${HOME} $$${X}");
    // a script keeps its lines
    expect(text).toContain("            - |\n              echo $${HOME} $$${X}\n              exit 0\n");
  });

  it("leaves out the CPU-binding annotations a template stamped: only a template may set them", () => {
    const d = dp();
    (d.spec.pod as unknown as { metadata: unknown }).metadata = { annotations: { "kore.zjusct.io/pin": "true", "kore.zjusct.io/pool": "a", note: "kept" }, labels: { team: "x" } };
    const text = devpodToConfig(d);
    expect(text).not.toContain("kore.zjusct.io");
    expect(text).toContain("        note: kept\n");
    expect(text).toContain("        team: x\n");
    // nothing but those annotations: the empty map goes too
    (d.spec.pod as unknown as { metadata: unknown }).metadata = { annotations: { "kore.zjusct.io/pin": "true" } };
    expect(devpodToConfig(d)).not.toMatch(/annotations|metadata: \{\}|kore/);
  });

  it("quotes the short words YAML 1.1 reads as booleans, and binary numbers", () => {
    const d = dp();
    (d.spec.pod!.spec!.containers![0] as unknown as { args: unknown[] }).args = ["y", "N", "0b101", "yes-sir"];
    const text = devpodToConfig(d);
    for (const q of ['- "y"', '- "N"', '- "0b101"', "- yes-sir"]) expect(text).toContain(q);
  });

  it("quotes what YAML would read as something else", () => {
    const d = dp();
    (d.spec.pod!.spec!.containers![0] as unknown as { args: unknown[] }).args = ["true", "1.5", "", "a: b", "# x", "- y", "null", " pad", "plain-text_1.0"];
    const text = devpodToConfig(d);
    for (const q of ['- "true"', '- "1.5"', '- ""', '- "a: b"', '- "# x"', '- "- y"', '- "null"', '- " pad"', "- plain-text_1.0"]) expect(text).toContain(q);
  });
});
