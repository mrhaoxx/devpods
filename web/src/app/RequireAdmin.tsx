import { Navigate, Outlet } from "react-router-dom";
import { useMe } from "../data/queries";

/** Admin-only routes: everyone else goes to /. */
export function RequireAdmin() {
  const me = useMe();
  if (!me.data) return null;
  return me.data.admin ? <Outlet /> : <Navigate to="/" replace />;
}

/** Audit routes exist only when the deployment records sessions. */
export function RequireAudit() {
  const me = useMe();
  if (!me.data) return null;
  return me.data.features.audit ? <Outlet /> : <Navigate to="/" replace />;
}
