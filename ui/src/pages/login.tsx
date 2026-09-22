import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { KeyRound, LogIn } from "lucide-react";
import { api } from "@/lib/api";
import { setToken } from "@/lib/auth";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Wordmark } from "@/components/brand";

export function LoginPage() {
  const [value, setValue] = useState("");
  const config = useQuery({ queryKey: ["config"], queryFn: api.config });
  const providers = config.data?.auth?.providers ?? [];
  const [showToken, setShowToken] = useState(providers.length === 0);
  const params = new URLSearchParams(window.location.search);
  const loginError = params.get("login_error");
  // After signing in, come back to the page the user asked for.
  const next =
    window.location.pathname === "/"
      ? "/"
      : window.location.pathname + window.location.search;
  const tokenAllowed = config.data?.auth?.token ?? true;
  const tokenForm = tokenAllowed && (showToken || providers.length === 0);

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>
            <Wordmark className="h-8" />
          </CardTitle>
          <CardDescription>
            {providers.length > 0 ? (
              <>
                Sign in to the dashboard with your account.
                {!tokenAllowed && " The admin token is disabled on this cluster."}
              </>
            ) : (
              <>
                Paste the admin token to open the dashboard. Get it with{" "}
                <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
                  shpyrd cluster token
                </code>
                , or run{" "}
                <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs">
                  shpyrd cluster dashboard
                </code>{" "}
                to be signed in automatically.
              </>
            )}
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          {loginError && (
            <Alert variant="destructive">
              <AlertTitle>Sign-in failed</AlertTitle>
              <AlertDescription>{loginError}</AlertDescription>
            </Alert>
          )}
          {providers.map((p) => (
            <Button key={p.id} asChild size="lg">
              <a href={api.loginUrl(p.id, next)}>
                <LogIn data-icon="inline-start" /> Sign in with{" "}
                {p.label.toLowerCase()}
              </a>
            </Button>
          ))}
          {providers.length > 0 && !showToken && tokenAllowed && (
            <button
              type="button"
              className="text-xs text-muted-foreground underline-offset-4 hover:underline"
              onClick={() => setShowToken(true)}
            >
              Use the admin token instead
            </button>
          )}
          {tokenForm && (
            <form
              className="grid gap-4"
              onSubmit={(e) => {
                e.preventDefault();
                if (value.trim()) setToken(value.trim());
              }}
            >
              <div className="grid gap-2">
                <Label htmlFor="token">Admin token</Label>
                <Input
                  id="token"
                  type="password"
                  autoComplete="off"
                  autoFocus={providers.length === 0}
                  value={value}
                  onChange={(e) => setValue(e.target.value)}
                  placeholder="64 hex characters"
                />
              </div>
              <Button
                type="submit"
                variant={providers.length > 0 ? "outline" : "default"}
                disabled={!value.trim()}
              >
                <KeyRound data-icon="inline-start" /> Sign in with the token
              </Button>
            </form>
          )}
          {config.data && !config.data.authRequired && (
            <p className="text-xs text-muted-foreground">
              This server does not require a token.
            </p>
          )}
          {config.data?.domain && (
            <p className="text-xs text-muted-foreground">
              Cluster domain {config.data.domain} · {config.data.version}
            </p>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
