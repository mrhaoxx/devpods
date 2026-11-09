import React from "react";
import ReactDOM from "react-dom/client";
import { createBrowserRouter, RouterProvider } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "./index.css";
import Login from "./pages/Login";
import PodList from "./pages/PodList";
import PodDetail from "./pages/PodDetail";
import PodCreate from "./pages/PodCreate";
import Pubkeys from "./pages/Pubkeys";
import Password from "./pages/Password";
import AdminUsers from "./pages/AdminUsers";
import AdminDevPods from "./pages/AdminDevPods";
import AdminTopology from "./pages/AdminTopology";

const qc = new QueryClient();
import AuditSessions from "./pages/audit/Sessions";
import AuditSessionDetail from "./pages/audit/SessionDetail";
import AuditCommands from "./pages/audit/Commands";
import AuditFiles from "./pages/audit/Files";
import AuditAlerts from "./pages/audit/Alerts";
import AuditAccessLog from "./pages/audit/AccessLog";

const router = createBrowserRouter([
  { path: "/login", element: <Login /> },
  { path: "/", element: <PodList /> },
  { path: "/devpods/new", element: <PodCreate /> },
  { path: "/devpods/:name", element: <PodDetail /> },
  { path: "/settings/pubkeys", element: <Pubkeys /> },
  { path: "/settings/password", element: <Password /> },
  { path: "/admin/users", element: <AdminUsers /> },
  { path: "/admin/devpods", element: <AdminDevPods /> },
  { path: "/admin/topology", element: <AdminTopology /> },
  { path: "/audit/sessions", element: <AuditSessions /> },
  { path: "/audit/sessions/:id", element: <AuditSessionDetail /> },
  { path: "/audit/commands", element: <AuditCommands /> },
  { path: "/audit/files", element: <AuditFiles /> },
  { path: "/audit/alerts", element: <AuditAlerts /> },
  { path: "/audit/access-log", element: <AuditAccessLog /> },
]);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </React.StrictMode>,
);
