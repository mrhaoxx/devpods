// CSV for the one-time password downloads (UI spec §5.5).
const cell = (v: string) => (/[",\r\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v);

export function toCSV(rows: string[][]): string {
  return rows.map((r) => r.map(cell).join(",") + "\r\n").join("");
}

export function csvDownload(rows: string[][]): Blob {
  return new Blob([toCSV(rows)], { type: "text/csv;charset=utf-8" });
}

/** saveBlob offers a Blob as a file download. */
export function saveBlob(b: Blob, filename: string) {
  const url = URL.createObjectURL(b);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
