import { BrowserRouter, Route, Routes } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { useToken } from '@/lib/auth'
import { Layout } from '@/components/layout'
import { LoginPage } from '@/pages/login'
import { AppsPage } from '@/pages/apps'
import { AppDetailPage } from '@/pages/app-detail'
import { ClusterPage } from '@/pages/cluster'

export default function App() {
  const token = useToken()
  const config = useQuery({ queryKey: ['config'], queryFn: api.config, staleTime: 60_000 })

  // Until we know whether the server wants a token, render nothing to
  // avoid flashing the login screen.
  if (config.isLoading) return null
  if (config.data?.authRequired && !token) return <LoginPage />

  return (
    <BrowserRouter>
      <Routes>
        <Route element={<Layout />}>
          <Route path="/" element={<AppsPage />} />
          <Route path="/apps/:ns/:name" element={<AppDetailPage />} />
          <Route path="/cluster" element={<ClusterPage />} />
          <Route path="*" element={<AppsPage />} />
        </Route>
      </Routes>
    </BrowserRouter>
  )
}
