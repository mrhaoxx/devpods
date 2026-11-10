import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";
import { ApiFailure } from "../data/api";
import { LiveHubProvider } from "../data/live/hooks";
import { appHub } from "./hub";
import { AppRoutes } from "./routes";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // client errors (403, 404, 409 …) are answers, not hiccups
      retry: (n, e) => !(e instanceof ApiFailure && e.status < 500) && n < 2,
      refetchOnWindowFocus: false,
    },
  },
});

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <LiveHubProvider hub={appHub} enabled={false}>
        <BrowserRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <AppRoutes />
        </BrowserRouter>
      </LiveHubProvider>
    </QueryClientProvider>
  );
}
