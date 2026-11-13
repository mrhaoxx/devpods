import { expect, it } from "vitest";
import type { ProcLive } from "../../../data/api";
import { procTree } from "./procTree";

const p = (pid: number, ppid?: number) => ({ pid, ppid, start: 1, exe: "", cmdline: "", cpu: 0, rss: 0, ioR: 0, ioW: 0, tcpRx: 0, tcpTx: 0 }) as ProcLive;

it("puts each process under its parent, children by pid; an unknown parent makes a root", () => {
  const rows = procTree([p(30, 10), p(10, 1), p(1), p(20, 10), p(40, 999), p(21, 20)]);
  expect(rows.map((r) => `${r.depth}:${r.p.pid}`)).toEqual(["0:1", "1:10", "2:20", "3:21", "2:30", "0:40"]);
});
