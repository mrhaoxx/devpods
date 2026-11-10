// Formatting helpers for the dense tables (pure; unit-tested).

const UNITS = ["K", "M", "G", "T", "P"];

// fmtBytes: 1024-based, one decimal: 1536 → "1.5K", 58.1Gi → "58.1G".
export function fmtBytes(n: number): string {
  if (!isFinite(n) || n < 1024) return String(Math.max(0, Math.round(n || 0)));
  let v = n;
  let i = -1;
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)}${UNITS[i]}`;
}

// fmtDuration shows the largest unit plus the next one when non-zero:
// 42s, 45m, 2h12m, 2d6h.
export function fmtDuration(ms: number): string {
  let s = Math.max(0, Math.floor(ms / 1000));
  const d = Math.floor(s / 86400);
  s -= d * 86400;
  const h = Math.floor(s / 3600);
  s -= h * 3600;
  const m = Math.floor(s / 60);
  s -= m * 60;
  if (d > 0) return h ? `${d}d${h}h` : `${d}d`;
  if (h > 0) return m ? `${h}h${m}m` : `${h}h`;
  if (m > 0) return s ? `${m}m${s}s` : `${m}m`;
  return `${s}s`;
}

export function fmtAge(iso: string, now: Date = new Date()): string {
  return fmtDuration(now.getTime() - new Date(iso).getTime());
}

// failureRetryIn mirrors the controller's failureBackoff
// (10s·2^(n-1), capped at 300s): seconds until the next retry.
export function failureRetryIn(retryCount: number, lastFailureAt: string, now: Date = new Date()): number {
  let backoff = 10;
  for (let i = 1; i < retryCount && backoff < 300; i++) backoff *= 2;
  backoff = Math.min(backoff, 300);
  const elapsed = (now.getTime() - new Date(lastFailureAt).getTime()) / 1000;
  return Math.max(0, Math.ceil(backoff - elapsed));
}

const SUFFIX: Record<string, number> = {
  "": 1,
  m: 1e-3,
  k: 1e3,
  M: 1e6,
  G: 1e9,
  T: 1e12,
  P: 1e15,
  Ki: 1024,
  Mi: 1024 ** 2,
  Gi: 1024 ** 3,
  Ti: 1024 ** 4,
  Pi: 1024 ** 5,
};

// parseQuantity understands the Kubernetes quantity forms used in limits.
export function parseQuantity(q?: string): number {
  if (!q) return 0;
  const m = /^([0-9.]+(?:e[+-]?\d+)?)([a-zA-Z]*)$/.exec(q.trim());
  if (!m || !(m[2] in SUFFIX)) return 0;
  return parseFloat(m[1]) * SUFFIX[m[2]];
}

type ContainerLike = { resources?: { limits?: Record<string, string | undefined> } };

// sumLimits adds the containers' cpu (cores), memory (bytes) and GPU limits.
export function sumLimits(spec?: { containers?: ContainerLike[] }): { cpu: number; memory: number; gpu: number } {
  const out = { cpu: 0, memory: 0, gpu: 0 };
  for (const c of spec?.containers ?? []) {
    const l = c.resources?.limits ?? {};
    out.cpu += parseQuantity(l.cpu);
    out.memory += parseQuantity(l.memory);
    out.gpu += parseQuantity(l["nvidia.com/gpu"]);
  }
  out.cpu = Math.round(out.cpu * 1000) / 1000;
  return out;
}

// fmtRate: bytes per second → "1.5M/s" (or "0/s").
export function fmtRate(bps: number): string {
  return `${fmtBytes(bps)}/s`;
}

// fmtCores: 0.5 → "0.5", 12 → "12", 7.43 → "7.4".
export function fmtCores(c: number): string {
  if (!isFinite(c)) return "—";
  return c >= 10 ? String(Math.round(c)) : String(+c.toFixed(1));
}
