import { describe, expect, it } from "vitest";
import { csvDownload, toCSV } from "./csv";

describe("csv", () => {
  it("csvDownload quotes commas, quotes and newlines", () => {
    expect(toCSV([["username", "password"], ["a,b", 'say "hi"'], ["x\ny", "plain"]])).toBe('username,password\r\n"a,b","say ""hi"""\r\n"x\ny",plain\r\n');
    const b = csvDownload([["a"]]);
    expect(b.type).toBe("text/csv;charset=utf-8");
  });
});
