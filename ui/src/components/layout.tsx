import { Link, NavLink, Outlet, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  ExternalLink,
  LogOut,
  Monitor,
  Moon,
  Sun,
  UserRound,
} from "lucide-react";
import { api } from "@/lib/api";
import { getToken, setToken } from "@/lib/auth";
import { usePerms } from "@/lib/me";
import { useTheme, type Theme } from "@/lib/theme";
import { LogoMark, Wordmark } from "@/components/brand";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { cn } from "@/lib/utils";

export function Layout() {
  const config = useQuery({
    queryKey: ["config"],
    queryFn: api.config,
    staleTime: 60_000,
  });
  const perms = usePerms();
  // The cluster is the operator's: only the console (the implicit
  // workspace's dashboard) shows it (RFC-0033 phase 6).
  const console = config.data?.workspace?.implicit !== false;
  const usersEnabled =
    config.data?.extensions?.includes("auth-local") && perms.clusterAdmin;

  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="border-b bg-card/40">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-6 px-4">
          <Link to="/" className="flex items-center" aria-label="shpyrd">
            <Wordmark className="hidden sm:block" />
            <LogoMark className="size-7 sm:hidden" />
          </Link>
          <nav className="flex items-center gap-1 text-sm">
            <NavItem to="/">Projects</NavItem>
            {perms.clusterView && console && (
              <NavItem to="/cluster">Cluster</NavItem>
            )}
            {(perms.clusterAdmin || usersEnabled) && (
              <NavItem to="/workspace">Workspace</NavItem>
            )}
          </nav>
          <div className="ml-auto flex items-center gap-1 text-sm text-muted-foreground">
            {config.data?.grafanaUrl && console && (
              <Button variant="ghost" size="sm" asChild>
                <a
                  href={config.data.grafanaUrl}
                  target="_blank"
                  rel="noreferrer"
                >
                  Grafana <ExternalLink data-icon="inline-end" />
                </a>
              </Button>
            )}
            <ThemeToggle />
            {config.data?.version && (
              <span className="hidden font-mono text-xs sm:inline">
                {config.data.version}
              </span>
            )}
            {config.data?.authRequired && <UserMenu />}
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-6xl px-4 py-6">
        <Outlet />
      </main>
    </div>
  );
}

// UserMenu shows who is signed in (a user through a login provider, or the
// admin token) and signs out.
function UserMenu() {
  const me = useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    staleTime: 60_000,
    retry: false,
  });
  const signOut = async () => {
    if (getToken()) {
      setToken(null);
      return;
    }
    // The server names the destination: the issuer's sign-out page when it
    // has one (RFC-0012), else the root.
    let to = "/";
    try {
      const r = await api.logout();
      if (r?.redirect) to = r.redirect;
    } finally {
      window.location.href = to;
    }
  };
  const label = me.data?.email || me.data?.name || "admin token";
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="sm" title={label}>
          <UserRound />
          <span className="hidden max-w-40 truncate md:inline">{label}</span>
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuLabel className="font-normal">
          <div className="text-sm font-medium">{me.data?.name || label}</div>
          {me.data?.email && (
            <div className="text-xs text-muted-foreground">{me.data.email}</div>
          )}
          <div className="text-xs text-muted-foreground">
            {me.data?.provider === "token"
              ? "admin token"
              : `signed in with ${me.data?.provider ?? "..."}`}
          </div>
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuItem onClick={signOut}>
          <LogOut /> Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

const themes: { value: Theme; label: string; icon: typeof Sun }[] = [
  { value: "light", label: "Light", icon: Sun },
  { value: "dark", label: "Dark", icon: Moon },
  { value: "system", label: "System", icon: Monitor },
];

/** Theme selector like the website's: Light, Dark or System. */
export function ThemeToggle() {
  const [theme, setTheme] = useTheme();
  const current = themes.find((t) => t.value === theme) ?? themes[2];
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="sm" aria-label="Theme">
          <current.icon />{" "}
          <span className="hidden md:inline">{current.label}</span>
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuLabel>Theme</DropdownMenuLabel>
        <DropdownMenuRadioGroup
          value={theme}
          onValueChange={(v) => setTheme(v as Theme)}
        >
          {themes.map((t) => (
            <DropdownMenuRadioItem key={t.value} value={t.value}>
              <t.icon className="mr-2 size-4" /> {t.label}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function NavItem({ to, children }: { to: string; children: React.ReactNode }) {
  const { pathname } = useLocation();
  // "Projects" owns both the list and the project pages.
  const active =
    to === "/"
      ? pathname === "/" || pathname.startsWith("/projects/")
      : pathname.startsWith(to);
  return (
    <NavLink
      to={to}
      className={cn(
        "rounded-md px-3 py-1.5 transition-colors hover:bg-accent hover:text-accent-foreground",
        active ? "bg-accent text-accent-foreground" : "text-muted-foreground",
      )}
    >
      {children}
    </NavLink>
  );
}
