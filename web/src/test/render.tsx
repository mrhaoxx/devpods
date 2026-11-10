// renderWithProviders mounts a component the way the app does: a fresh
// QueryClient (no retries), a MemoryRouter at `route`, and a stub `me`
// already in the query cache.
import type { ReactElement } from "react";
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
