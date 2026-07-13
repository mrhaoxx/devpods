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
]);

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </React.StrictMode>,
);
