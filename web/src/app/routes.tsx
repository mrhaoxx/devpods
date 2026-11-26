// Route table (UI spec §4.2). Pages whose console version has not landed
// yet still render their phase-1 component inside the shell.
import { ClustersPage } from "../features/clusters/ClustersPage";
import { Navigate, Route, Routes, useParams } from "react-router-dom";
import { Shell } from "./Shell";
import { LoginPage } from "./LoginPage";
import { RequireAdmin, RequireAudit, RequirePermission } from "./RequireAdmin";
import { SettingsPage } from "../features/settings/SettingsPage";
import { MyDevPodsPage } from "../features/devpods/MyDevPodsPage";
import { AdminDevPodsPage } from "../features/devpods/AdminDevPodsPage";
import { DetailPage } from "../features/devpods/detail/DetailPage";
import { OverviewPage } from "../features/overview/OverviewPage";
import { UsersPage } from "../features/users/UsersPage";
import { SessionsPage } from "../features/audit/SessionsPage";
import { SessionPage } from "../features/audit/SessionPage";
import { CommandsPage, EventsPage, FilesPage, HTTPPage } from "../features/audit/SearchPages";
import { AccessLogPage, AlertsPage, PolicyPage } from "../features/audit/AdminPages";
import { NodesPage } from "../features/nodes/NodesPage";
import { ManagedNodesPage } from "../features/nodes/ManagedNodesPage";
import { HttpAuthPage } from "../features/devpods/HttpAuthPage";
import { ReservationsPage } from "../features/reservations/ReservationsPage";
import { QueuePage } from "../features/queue/QueuePage";
import { JobPage } from "../features/queue/JobPage";
import { AccessPage } from "../features/access/AccessPage";
import { GroupsPage } from "../features/access/GroupsPage";
import { AdminReservationsPage } from "../features/reservations/AdminReservationsPage";
import { ImagesPage } from "../features/images/ImagesPage";
import { ShellPage } from "../features/shell/ShellPage";
import { ConfigsPage } from "../features/configs/ConfigsPage";
import { ConfigEditor } from "../features/configs/ConfigEditor";

function SessionRedirect() {
  const { id } = useParams();
  return <Navigate to={`/sessions/${id}`} replace />;
}

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      {/* the web terminal fills its own tab, outside the console shell */}
      <Route path="/devpods/:cluster/:name/shell" element={<ShellPage />} />
      <Route element={<Shell />}>
        <Route index element={<MyDevPodsPage />} />
        <Route path="devpods/new" element={<Navigate to="/?new=1" replace />} />
        <Route path="devpods/:cluster/:name" element={<DetailPage />} />
        <Route path="http-auth" element={<HttpAuthPage />} />
        <Route path="reservations" element={<ReservationsPage />} />
        <Route path="queue" element={<QueuePage />} />
        <Route path="queue/:cluster/:id" element={<JobPage />} />
        <Route path="images" element={<ImagesPage />} />
        <Route path="configs" element={<ConfigsPage />} />
        <Route path="configs/new" element={<ConfigEditor key="new" />} />
        <Route path="configs/:name" element={<ConfigEditor />} />
        <Route path="settings" element={<SettingsPage />} />
        <Route element={<RequirePermission p="setReservable" />}>
          <Route path="nodes/managed" element={<ManagedNodesPage />} />
        </Route>
        <Route path="settings/pubkeys" element={<Navigate to="/settings" replace />} />
        <Route path="settings/password" element={<Navigate to="/settings" replace />} />
        <Route element={<RequireAudit />}>
          <Route path="sessions" element={<SessionsPage mine />} />
          <Route path="sessions/:id" element={<SessionPage />} />
          <Route path="audit/sessions/:id" element={<SessionRedirect />} />
          <Route path="audit/commands" element={<CommandsPage />} />
          <Route path="audit/files" element={<FilesPage />} />
          <Route path="audit/http" element={<HTTPPage />} />
          <Route path="audit/events" element={<EventsPage />} />
          <Route element={<RequirePermission p="viewAudit" />}>
            <Route path="audit/sessions" element={<SessionsPage />} />
            <Route path="audit/alerts" element={<AlertsPage />} />
          </Route>
          <Route element={<RequireAdmin />}>
            <Route path="audit/access-log" element={<AccessLogPage />} />
            <Route path="audit/policy" element={<PolicyPage />} />
          </Route>
        </Route>
        <Route element={<RequireAdmin />}>
          <Route path="admin" element={<OverviewPage />} />
          <Route path="admin/devpods" element={<AdminDevPodsPage />} />
          <Route path="admin/nodes" element={<NodesPage />} />
          <Route path="admin/topology" element={<Navigate to="/admin/nodes" replace />} />
          <Route path="admin/users" element={<UsersPage />} />
          <Route path="admin/clusters" element={<ClustersPage />} />
          <Route path="admin/images" element={<ImagesPage everyone />} />
          <Route path="admin/configs" element={<ConfigsPage everyone />} />
          <Route path="admin/reservations" element={<AdminReservationsPage />} />
          <Route path="admin/access" element={<AccessPage />} />
          <Route path="admin/groups" element={<GroupsPage />} />
        </Route>
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
