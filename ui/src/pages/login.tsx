import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { setToken } from '@/lib/auth'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Wordmark } from '@/components/brand'

export function LoginPage() {
  const [value, setValue] = useState('')
  const config = useQuery({ queryKey: ['config'], queryFn: api.config })

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>
            <Wordmark className="h-8" />
          </CardTitle>
          <CardDescription>
            Paste the admin token to open the dashboard. Get it with{' '}
            <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">shpyrd cluster token</code>, or run{' '}
            <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">shpyrd cluster dashboard</code> to be
            signed in automatically.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <form
            className="grid gap-4"
            onSubmit={(e) => {
              e.preventDefault()
              if (value.trim()) setToken(value.trim())
            }}
          >
            <div className="grid gap-2">
              <Label htmlFor="token">Admin token</Label>
              <Input
                id="token"
                type="password"
                autoComplete="off"
                autoFocus
                value={value}
                onChange={(e) => setValue(e.target.value)}
                placeholder="64 hex characters"
              />
            </div>
            <Button type="submit" disabled={!value.trim()}>
              Sign in
            </Button>
            {config.data && !config.data.authRequired && (
              <p className="text-xs text-muted-foreground">This server does not require a token.</p>
            )}
            {config.data?.domain && (
              <p className="text-xs text-muted-foreground">
                Cluster domain {config.data.domain} · {config.data.version}
              </p>
            )}
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
