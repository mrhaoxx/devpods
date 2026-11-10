// renderWithProviders mounts a component the way the app does: a fresh
// QueryClient (no retries), a MemoryRouter at `route`, and a stub `me`
// already in the query cache.
import type { ReactElement } from "react";
import { vi } from "vitest";
import { render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import type { Me } from "../api";
import { LiveHub } from "../data/live/LiveHub";
import { LiveHubProvider } from "../data/live/hooks";

export const stubMe = (over: Partial<Me> = {}): Me => ({
  user: "alice",
  admin: false,
  nameBudget: 40,
  quota: {},
  usage: { devpods: 0, running: 0, compute: {}, storage: "0" },
  features: { pubkeySelfService: true, kore: false, passwordAuth: true, audit: true },
  hasPassword: true,
  ssh: { host: "gw.example.com", port: 2222 },
  ...over,
});

/** hub: a LiveHub (over a fake OpenSSE) for components with live data;
 *  without one, live data is disabled. */
export type RenderOptions = { route?: string; me?: Me | null; client?: QueryClient; hub?: LiveHub; liveEnabled?: boolean };

export function renderWithProviders(ui: ReactElement, opts: RenderOptions = {}) {
  const client =
    opts.client ??
    new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity }, mutations: { retry: false } } });
  if (opts.me !== null) client.setQueryData(["me"], opts.me ?? stubMe());
  const result = render(
    <QueryClientProvider client={client}>
      <LiveHubProvider hub={opts.hub ?? new LiveHub({ open: () => () => {} })} enabled={opts.liveEnabled ?? !!opts.hub}>
        <MemoryRouter initialEntries={[opts.route ?? "/"]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          {ui}
        </MemoryRouter>
      </LiveHubProvider>
    </QueryClientProvider>,
  );
  return { ...result, client };
}

type Stub = unknown | ((url: URL, init?: RequestInit) => unknown);

/** stubFetch answers API calls from routes keyed "METHOD /path" or "/path"
 *  (any method; the query string is ignored); a value is sent as JSON,
 *  a Response as is; anything else is a 404. Returns the calls made. */
export function stubFetch(routes: Record<string, Stub>) {
  const calls: { method: string; url: URL; body?: string }[] = [];
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://test");
    const method = (init?.method ?? "GET").toUpperCase();
    calls.push({ method, url, body: typeof init?.body === "string" ? init.body : undefined });
    const hit = routes[`${method} ${url.pathname}`] ?? routes[url.pathname];
    if (hit === undefined) return new Response(JSON.stringify({ code: "NOT_FOUND", message: `no stub for ${method} ${url.pathname}` }), { status: 404 });
    const v = typeof hit === "function" ? (hit as (u: URL, i?: RequestInit) => unknown)(url, init) : hit;
    if (v instanceof Response) return v;
    return new Response(JSON.stringify(v), { status: 200, headers: { "Content-Type": "application/json" } });
  });
  return calls;
}
