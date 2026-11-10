// Route table (UI spec §4.2). Pages whose console version has not landed
// yet still render their phase-1 component inside the shell.
import { Navigate, Route, Routes, useParams } from "react-router-dom";
import { Shell } from "./Shell";
import { LoginPage } from "./LoginPage";
import { RequireAdmin, RequireAudit } from "./RequireAdmin";
import { SettingsPage } from "../features/settings/SettingsPage";
import { EmptyState } from "../design/EmptyState";
import { MyDevPodsPage } from "../features/devpods/MyDevPodsPage";
import { AdminDevPodsPage } from "../features/devpods/AdminDevPodsPage";
import { DetailPage } from "../features/devpods/detail/DetailPage";
import AdminUsers from "../pages/AdminUsers";
import AdminTopology from "../pages/AdminTopology";
import AuditSessions from "../pages/audit/Sessions";
import AuditSessionDetail from "../pages/audit/SessionDetail";
import AuditCommands from "../pages/audit/Commands";
import AuditFiles from "../pages/audit/Files";
import AuditAlerts from "../pages/audit/Alerts";
import AuditAccessLog from "../pages/audit/AccessLog";

function SessionRedirect() {
  const { id } = useParams();
  return <Navigate to={`/sessions/${id}`} replace />;
}

const Soon = ({ title }: { title: string }) => <EmptyState title={title} hint="即将上线" />;

export function AppRoutes() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route element={<Shell />}>
        <Route index element={<MyDevPodsPage />} />
        <Route path="devpods/new" element={<Navigate to="/?new=1" replace />} />
        <Route path="devpods/:name" element={<DetailPage />} />
        <Route path="settings" element={<SettingsPage />} />
        <Route path="settings/pubkeys" element={<Navigate to="/settings" replace />} />
        <Route path="settings/password" element={<Navigate to="/settings" replace />} />
        <Route element={<RequireAudit />}>
          <Route path="sessions" element={<AuditSessions />} />
          <Route path="sessions/:id" element={<AuditSessionDetail />} />
          <Route path="audit/sessions/:id" element={<SessionRedirect />} />
          <Route path="audit/commands" element={<AuditCommands />} />
          <Route path="audit/files" element={<AuditFiles />} />
          <Route element={<RequireAdmin />}>
            <Route path="audit/sessions" element={<AuditSessions />} />
            <Route path="audit/alerts" element={<AuditAlerts />} />
            <Route path="audit/access-log" element={<AuditAccessLog />} />
            <Route path="audit/policy" element={<Soon title="审计策略" />} />
          </Route>
        </Route>
        <Route element={<RequireAdmin />}>
          <Route path="admin" element={<Soon title="集群总览" />} />
          <Route path="admin/devpods" element={<AdminDevPodsPage />} />
          <Route path="admin/nodes" element={<AdminTopology />} />
          <Route path="admin/topology" element={<Navigate to="/admin/nodes" replace />} />
          <Route path="admin/users" element={<AdminUsers />} />
        </Route>
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
