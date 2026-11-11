import { describe, expect, it } from "vitest";
import { cleanRegistryHost, imageRegistryHost, REGISTRY_HOST_RE, sameRegistry, savedRegistryFor } from "./registry";

describe("registry hosts", () => {
  it("imageRegistryHost: a first part with a dot, a port or localhost is the registry", () => {
    expect(imageRegistryHost("Harbor.Example.com/team/app:v1")).toBe("harbor.example.com");
    expect(imageRegistryHost("reg:5000/app:v1")).toBe("reg:5000");
    expect(imageRegistryHost("localhost/app:v1")).toBe("localhost");
    expect(imageRegistryHost("team/app:v1")).toBe("docker.io");
    expect(imageRegistryHost("ubuntu:24.04")).toBe("docker.io");
  });
  it("savedRegistryFor matches by host; Docker Hub by any of its names", () => {
    const saved = [{ host: "harbor.example.com", username: "bot" }, { host: "index.docker.io", username: "me" }];
    expect(savedRegistryFor("harbor.example.com/team/app:v1", saved)?.username).toBe("bot");
    expect(savedRegistryFor("HARBOR.example.com/team/app:v1", saved)?.username).toBe("bot");
    expect(savedRegistryFor("team/app:v1", saved)?.username).toBe("me");
    expect(savedRegistryFor("other.test/team/app:v1", saved)).toBeUndefined();
    expect(savedRegistryFor("harbor.example.com", saved)).toBeUndefined(); // still typing
    expect(sameRegistry("docker.io", "registry-1.docker.io")).toBe(true);
    expect(sameRegistry("a.test", "b.test")).toBe(false);
  });
  it("cleanRegistryHost drops the scheme and path of a pasted address", () => {
    expect(cleanRegistryHost(" https://Harbor.Example.com/harbor/projects ")).toBe("harbor.example.com");
    expect(cleanRegistryHost("reg.test:5000/v2/")).toBe("reg.test:5000");
    expect(cleanRegistryHost("reg.test")).toBe("reg.test");
    expect(REGISTRY_HOST_RE.test("reg.test:5000")).toBe(true);
    expect(REGISTRY_HOST_RE.test("reg test")).toBe(false);
    expect(REGISTRY_HOST_RE.test("")).toBe(false);
  });
});
