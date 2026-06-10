import { Navigate, Route, Routes } from "react-router-dom";
import { useMe } from "./api/hooks";
import { SessionExpiredError } from "./api/client";
import { Login } from "./routes/Login";
import { Dashboard } from "./routes/Dashboard";

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/" element={<RequireAuth />} />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

// RequireAuth boots by querying /api/me. A 401 redirects to /login; otherwise
// the dashboard renders. This is the single auth gate for the SPA.
function RequireAuth() {
  const { data, isLoading, error } = useMe();

  if (isLoading) {
    return <div className="centered">Loading…</div>;
  }
  if (error instanceof SessionExpiredError) {
    return <Navigate to="/login" replace />;
  }
  if (error) {
    return <div className="centered">Something went wrong. Please reload.</div>;
  }
  if (!data) {
    return <Navigate to="/login" replace />;
  }
  return <Dashboard me={data} />;
}
