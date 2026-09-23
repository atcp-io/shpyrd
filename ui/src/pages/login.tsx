import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Building2, KeyRound, Loader2, LogIn } from "lucide-react";
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

/**
 * The sign-in page (RFC-0012). Renders in place of the app whenever the
 * server wants a signed-in user: email and password when a provider
 * accepts them, a button per external provider, and the admin token only
 * when the cluster still accepts it.
 */
export function LoginPage() {
  const config = useQuery({ queryKey: ["config"], queryFn: api.config });
  const auth = config.data?.auth;
  const providers = auth?.providers ?? [];
  const password = auth?.password;
  const tokenAllowed = auth?.token ?? true;
  const accounts = !!password || providers.length > 0;
  const [showToken, setShowToken] = useState(false);
  const tokenForm = tokenAllowed && (showToken || !accounts);

  const params = new URLSearchParams(window.location.search);
  const redirectError = params.get("login_error");
  // After signing in, come back to the page the user asked for.
  const next =
    window.location.pathname === "/"
      ? "/"
      : window.location.pathname + window.location.search;

  if (config.isLoading) return null;

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>
            <Wordmark className="h-8" />
          </CardTitle>
          <CardDescription>
            {accounts ? (
              <>
                Sign in to shpyrd
                {config.data?.domain ? ` on ${config.data.domain}` : ""}.
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
          {redirectError && (
            <Alert variant="destructive">
              <AlertTitle>Sign-in failed</AlertTitle>
              <AlertDescription>{redirectError}</AlertDescription>
            </Alert>
          )}
          {password && <PasswordForm next={next} />}
          {password && providers.length > 0 && <Divider label="or" />}
          {providers.map((p) => (
            <Button
              key={p.id}
              asChild
              size="lg"
              variant={password ? "outline" : "default"}
            >
              <a href={api.loginUrl(p.id, next)}>
                <ProviderIcon kind={p.kind} /> Sign in with {p.label}
              </a>
            </Button>
          ))}
          {accounts && !showToken && tokenAllowed && (
            <button
              type="button"
              className="text-xs text-muted-foreground underline-offset-4 hover:underline"
              onClick={() => setShowToken(true)}
            >
              Use the admin token instead
            </button>
          )}
          {tokenForm && <TokenForm secondary={accounts} />}
          {config.data && !config.data.authRequired && (
            <p className="text-xs text-muted-foreground">
              This server does not require a token.
            </p>
          )}
          {config.data?.version && (
            <p className="text-xs text-muted-foreground">
              shpyrd {config.data.version}
            </p>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function PasswordForm({ next }: { next: string }) {
  const [email, setEmail] = useState("");
  const [pw, setPw] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!email.trim() || !pw) return;
    setBusy(true);
    setError(null);
    try {
      const r = await api.passwordLogin({ email: email.trim(), password: pw, next });
      window.location.assign(r.next || "/");
    } catch (err) {
      setError((err as Error).message);
      setPw("");
      setBusy(false);
    }
  };

  return (
    <form className="grid gap-4" onSubmit={submit} noValidate>
      <div className="grid gap-2">
        <Label htmlFor="email">Email</Label>
        <Input
          id="email"
          type="email"
          autoComplete="username"
          autoFocus
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="you@example.com"
        />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="password">Password</Label>
        <Input
          id="password"
          type="password"
          autoComplete="current-password"
          value={pw}
          onChange={(e) => setPw(e.target.value)}
          aria-invalid={!!error}
        />
      </div>
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
      <Button type="submit" size="lg" disabled={busy || !email.trim() || !pw}>
        {busy ? (
          <Loader2 className="animate-spin" data-icon="inline-start" />
        ) : (
          <LogIn data-icon="inline-start" />
        )}
        Sign in
      </Button>
    </form>
  );
}

function TokenForm({ secondary }: { secondary: boolean }) {
  const [value, setValue] = useState("");
  return (
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
          autoFocus={!secondary}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder="64 hex characters"
        />
      </div>
      <Button
        type="submit"
        variant={secondary ? "outline" : "default"}
        disabled={!value.trim()}
      >
        <KeyRound data-icon="inline-start" /> Sign in with the token
      </Button>
    </form>
  );
}

function Divider({ label }: { label: string }) {
  return (
    <div className="flex items-center gap-3 text-xs text-muted-foreground">
      <span className="h-px flex-1 bg-border" />
      {label}
      <span className="h-px flex-1 bg-border" />
    </div>
  );
}

/** Brand marks for the providers Dex connectors bring; a generic icon for
 * any other OpenID Connect issuer. */
function ProviderIcon({ kind }: { kind?: string }) {
  switch (kind) {
    case "github":
      return (
        <svg
          data-icon="inline-start"
          viewBox="0 0 24 24"
          fill="currentColor"
          aria-hidden="true"
        >
          <path d="M12 .5A11.5 11.5 0 0 0 .5 12c0 5.08 3.29 9.39 7.86 10.91.58.1.79-.25.79-.56v-2.1c-3.2.7-3.87-1.37-3.87-1.37-.52-1.33-1.28-1.68-1.28-1.68-1.04-.71.08-.7.08-.7 1.15.08 1.76 1.19 1.76 1.19 1.03 1.76 2.69 1.25 3.35.96.1-.75.4-1.25.73-1.54-2.55-.29-5.24-1.28-5.24-5.68 0-1.26.45-2.28 1.19-3.09-.12-.29-.52-1.46.11-3.05 0 0 .97-.31 3.17 1.18a11 11 0 0 1 5.78 0c2.2-1.49 3.17-1.18 3.17-1.18.63 1.59.23 2.76.11 3.05.74.81 1.19 1.83 1.19 3.09 0 4.41-2.69 5.38-5.26 5.67.41.36.78 1.06.78 2.14v3.17c0 .31.2.67.8.56A11.5 11.5 0 0 0 23.5 12 11.5 11.5 0 0 0 12 .5Z" />
        </svg>
      );
    case "google":
      return (
        <svg data-icon="inline-start" viewBox="0 0 24 24" aria-hidden="true">
          <path
            fill="#4285F4"
            d="M23.5 12.27c0-.85-.08-1.67-.22-2.45H12v4.64h6.45a5.5 5.5 0 0 1-2.39 3.62v3h3.86c2.26-2.08 3.58-5.15 3.58-8.81Z"
          />
          <path
            fill="#34A853"
            d="M12 24c3.24 0 5.96-1.07 7.94-2.91l-3.86-3a7.2 7.2 0 0 1-10.75-3.78H1.34v3.09A12 12 0 0 0 12 24Z"
          />
          <path
            fill="#FBBC04"
            d="M5.33 14.31A7.2 7.2 0 0 1 5.33 9.7V6.6H1.34a12 12 0 0 0 0 10.8l3.99-3.09Z"
          />
          <path
            fill="#EA4335"
            d="M12 4.77c1.76 0 3.34.6 4.59 1.79l3.42-3.42A11.5 11.5 0 0 0 12 0 12 12 0 0 0 1.34 6.6l3.99 3.1A7.17 7.17 0 0 1 12 4.77Z"
          />
        </svg>
      );
    default:
      return <Building2 data-icon="inline-start" />;
  }
}
