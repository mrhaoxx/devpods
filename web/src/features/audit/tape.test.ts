import { describe, expect, it } from "vitest";
import { parseLine, Tape } from "./tape";

describe("parseLine", () => {
  it("reads asciicast headers, events and the live markers", () => {
    expect(parseLine('{"version":2,"width":100,"height":30,"timestamp":1,"start":10,"now":42.5}')).toEqual({ kind: "header", cols: 100, rows: 30, start: 10000, now: 42500 });
    expect(parseLine('{"version":2,"width":80,"height":24}')).toEqual({ kind: "header", cols: 80, rows: 24, start: 0, now: undefined });
    expect(parseLine('[1.5,"o","hi"]')).toEqual({ kind: "event", ev: { t: 1500, o: "hi" } });
    expect(parseLine('[2,"r","120x40"]')).toEqual({ kind: "event", ev: { t: 2000, size: { cols: 120, rows: 40 } } });
    expect(parseLine('[2,"i","x"]')).toEqual({ kind: "skip" });
    expect(parseLine('{"live":true}')).toEqual({ kind: "live" });
    expect(parseLine('{"done":true}')).toEqual({ kind: "done" });
    expect(parseLine('{"offline":"overflow"}')).toEqual({ kind: "offline", why: "overflow" });
    expect(parseLine('{"hb":true}')).toEqual({ kind: "skip" });
    expect(parseLine("garbage")).toEqual({ kind: "skip" });
  });
});

const fill = (t: Tape) => {
  t.push({ t: 0, size: { cols: 100, rows: 30 } });
  t.push({ t: 1000, o: "a" });
  t.push({ t: 2000, o: "b" });
  t.push({ t: 3000, size: { cols: 120, rows: 40 } });
  t.push({ t: 4000, o: "c" });
};

describe("Tape", () => {
  it("rebuilds the screen at a moment: size, then output and resizes in order", () => {
    const t = new Tape({ cols: 80, rows: 24 });
    fill(t);
    expect(t.frameAt(500)).toEqual({ size: { cols: 100, rows: 30 }, ops: [], next: 1 });
    expect(t.frameAt(2500)).toEqual({ size: { cols: 100, rows: 30 }, ops: ["ab"], next: 3 });
    expect(t.frameAt(4000)).toEqual({ size: { cols: 100, rows: 30 }, ops: ["ab", { cols: 120, rows: 40 }, "c"], next: 5 });
    expect(t.end).toBe(4000);
  });
  it("replays at most feedBytes of output for a frame", () => {
    const t = new Tape({ cols: 80, rows: 24 }, 0, { feedBytes: 2 });
    fill(t);
    expect(t.frameAt(4000)).toEqual({ size: { cols: 100, rows: 30 }, ops: ["b", { cols: 120, rows: 40 }, "c"], next: 5 });
  });
  it("drops the oldest output past maxBytes and moves its start", () => {
    const t = new Tape({ cols: 80, rows: 24 }, 0, { maxBytes: 4, keepBytes: 2 });
    expect(t.push({ t: 1000, o: "aa" })).toBe(0);
    t.push({ t: 2000, size: { cols: 90, rows: 20 } });
    t.push({ t: 2500, o: "bb" });
    expect(t.push({ t: 4000, o: "cc" })).toBe(3);
    expect(t.start).toBe(4000);
    expect(t.frameAt(4000)).toEqual({ size: { cols: 90, rows: 20 }, ops: ["cc"], next: 1 });
  });
});
