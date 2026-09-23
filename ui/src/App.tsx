import { BrowserRouter, Route, Routes } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { useToken } from "@/lib/auth";
import { Layout } from "@/components/layout";
import { LoginPage } from "@/pages/login";
import { AppsPage } from "@/pages/apps";
import { AppDetailPage } from "@/pages/app-detail";
import { ClusterPage } from "@/pages/cluster";
import { UsersPage } from "@/pages/users";
import { TeamsPage } from "@/pages/teams";

export default function App() {
  const token = useToken();
  const config = useQuery({
    queryKey: ["config"],
    queryFn: api.config,
    staleTime: 60_000,
  });
  // Without a token, a session cookie may still sign us in (RFC-0007).
  const me = useQuery({
    queryKey: ["me", "cookie"],
    queryFn: api.me,
    enabled: !!config.data?.authRequired && !token,
    retry: false,
    staleTime: 60_000,
  });

  // Until we know whether the server wants a token, render nothing to
  // avoid flashing the login screen.
  if (config.isLoading) return null;
  if (config.data?.authRequired && !token) {
    if (me.isLoading) return null;
    if (!me.data) return <LoginPage />;
  }

  return (
    <BrowserRouter>
      <Routes>
        <Route element={<Layout />}>
          <Route path="/" element={<AppsPage />} />
          <Route path="/projects/:slug" element={<AppDetailPage />} />
          <Route path="/cluster" element={<ClusterPage />} />
          <Route path="/users" element={<UsersPage />} />
          <Route path="/teams" element={<TeamsPage />} />
          <Route path="*" element={<AppsPage />} />
        </Route>
      </Routes>
    </BrowserRouter>
  );
}
