import { describe, expect, it } from "vitest";
import { stubMe } from "../test/render";
import { sshCommand, sshConfig } from "./api";

// The login a copied command uses: the owner's is <owner>+<the name after
// "<owner>-">; anyone else (a collaborator) logs in as themselves, with the
// DevPod's whole name — the gateway finds it by that and lets them in.
describe("ssh login", () => {
  const alice = stubMe({ user: "alice" });
  const bob = stubMe({ user: "bob", clusters: { st2: { quota: {}, usage: { devpods: 0, running: 0, compute: {}, storage: "0" }, ssh: { host: "gw2", port: 22, loginSuffix: "st2" } } } });

  it("the owner: their name and the short name", () => {
    expect(sshCommand(alice, "alice", "alice-sg")).toBe("ssh -p 2222 alice+sg@gw.example.com");
    expect(sshCommand(alice, "alice", "day2")).toBe("ssh -p 2222 alice+day2@gw.example.com"); // named before owner prefixes
  });

  it("a collaborator: their own name and the whole DevPod name, in its cluster", () => {
    expect(sshCommand(bob, "alice", "alice-sg")).toBe("ssh -p 2222 bob+alice-sg@gw.example.com");
    expect(sshCommand(bob, "alice", "alice-sg", "st2")).toBe("ssh bob+alice-sg+st2@gw2");
  });

  it("the ssh config: Host is the DevPod's name, User the login", () => {
    expect(sshConfig(alice, "alice", "alice-sg")).toBe("Host alice-sg\n    HostName gw.example.com\n    Port 2222\n    User alice+sg");
    expect(sshConfig(bob, "alice", "alice-sg")).toBe("Host alice-sg\n    HostName gw.example.com\n    Port 2222\n    User bob+alice-sg");
  });
});
