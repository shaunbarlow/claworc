/**
 * Integration tests for the QMD memory-backend binary baked into the agent
 * image (agent/instance/Dockerfile). The control plane opt-ins an agent into
 * QMD by pushing memory.backend=qmd into openclaw.json; OpenClaw then spawns
 * `qmd` from the gateway process's PATH. These tests guard the install:
 * binary present, on PATH for both exec and login-shell paths, and runnable
 * by the claworc user.
 */
import { describe, it, expect } from "vitest";
import { exec, execAsUser, getContainers } from "./helpers";

const containers = getContainers();
// qmd ships in the claworc-agent image only (browser images don't run the
// gateway and never need it).
const container = containers.agent?.name;

describe.skipIf(!container)("qmd memory backend binary", { timeout: 120_000 }, () => {
  it("qmd is on the default PATH (docker exec)", () => {
    const result = exec(container!, ["qmd", "--version"]);
    expect(result.exitCode).toBe(0);
    expect(result.stdout.trim()).not.toBe("");
  });

  it("qmd resolves at /usr/local/bin for the gateway process PATH", () => {
    const result = exec(container!, ["readlink", "-f", "/usr/local/bin/qmd"]);
    expect(result.exitCode).toBe(0);
  });

  it("claworc user can run qmd from a login shell", () => {
    const result = execAsUser(container!, "qmd --version");
    expect(result.exitCode).toBe(0);
  });
});
