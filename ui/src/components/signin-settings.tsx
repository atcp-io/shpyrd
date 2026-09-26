import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus, ShieldCheck, Trash2 } from "lucide-react";

import { api, type DomainClaim } from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

/**
 * How people sign in to this workspace (RFC-0033 phase 3): the login
 * methods, who may join on first sign-in, and the email domains the company
 * owns.
 */
export function SignInSettings({ authLocal }: { authLocal: boolean }) {
  return (
    <div className="grid gap-6">
      {authLocal ? (
        <LoginMethodsCard />
      ) : (
        <Card>
          <CardHeader>
            <CardTitle>Login methods</CardTitle>
            <CardDescription>
              Enable the <code>auth-local</code> extension to manage sign-in
              methods here (<code>shpyrd extensions enable auth-local</code>).
            </CardDescription>
          </CardHeader>
        </Card>
      )}
      <JoinPolicyCard />
      <DomainClaimsCard />
    </div>
  );
}

const kindLabels: Record<string, string> = {
  google: "Google",
  microsoft: "Microsoft",
  github: "GitHub",
  oidc: "OpenID Connect (Okta, Keycloak, Auth0…)",
};

function LoginMethodsCard() {
  const qc = useQueryClient();
  const methods = useQuery({
    queryKey: ["login-methods"],
    queryFn: api.loginMethods,
    retry: false,
  });
  const remove = useMutation({
    mutationFn: (id: string) => api.removeConnector(id),
    onSuccess: (_, id) => {
      toast.success(`Removed ${id}`);
      qc.invalidateQueries({ queryKey: ["login-methods"] });
      qc.invalidateQueries({ queryKey: ["config"] });
    },
    onError: (e: Error) => toast.error(e.message),
  });
  return (
    <Card>
      <CardHeader>
        <CardTitle>Login methods</CardTitle>
        <CardDescription>
          The ways people sign in to this workspace — and to every app behind
          sign-in. Add your company's identity provider so nobody needs another
          password; groups of the provider map to teams.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {methods.isLoading && <Skeleton className="h-16 w-full" />}
        {methods.error && (
          <p className="text-sm text-destructive">
            {(methods.error as Error).message}
          </p>
        )}
        {methods.data && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Method</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Restriction</TableHead>
                <TableHead className="w-24" />
              </TableRow>
            </TableHeader>
            <TableBody>
              {methods.data.password && (
                <TableRow>
                  <TableCell className="font-medium">
                    Email and password
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    local accounts
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    Accounts tab
                  </TableCell>
                  <TableCell />
                </TableRow>
              )}
              {methods.data.connectors.map((c) => (
                <TableRow key={c.id}>
                  <TableCell className="font-medium">
                    {c.name}{" "}
                    <span className="font-mono text-xs text-muted-foreground">
                      {c.id}
                    </span>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {kindLabels[c.type] ?? c.type}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {c.detail || "—"}
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="xs"
                      className="text-destructive"
                      disabled={remove.isPending}
                      onClick={() => {
                        if (
                          window.confirm(
                            `Remove the ${c.name} sign-in method? People signed in through it keep their sessions.`,
                          )
                        )
                          remove.mutate(c.id);
                      }}
                    >
                      <Trash2 data-icon="inline-start" /> Remove
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {methods.data && (
          <AddConnectorDialog
            kinds={methods.data.kinds}
            callback={methods.data.callback}
            onDone={() => {
              qc.invalidateQueries({ queryKey: ["login-methods"] });
              qc.invalidateQueries({ queryKey: ["config"] });
            }}
          />
        )}
      </CardContent>
    </Card>
  );
}

function AddConnectorDialog({
  kinds,
  callback,
  onDone,
}: {
  kinds: string[];
  callback: string;
  onDone: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [type, setType] = useState("google");
  const [form, setForm] = useState({
    id: "",
    name: "",
    clientId: "",
    clientSecret: "",
    org: "",
    hostedDomain: "",
    tenant: "",
    issuer: "",
  });
  const set =
    (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) =>
      setForm({ ...form, [k]: e.target.value });
  const add = useMutation({
    mutationFn: () => api.addConnector({ type, ...form }),
    onSuccess: () => {
      toast.success(
        "Sign-in method added; the button is on the login page now",
      );
      setOpen(false);
      setForm({
        id: "",
        name: "",
        clientId: "",
        clientSecret: "",
        org: "",
        hostedDomain: "",
        tenant: "",
        issuer: "",
      });
      onDone();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline" className="justify-self-start">
          <Plus data-icon="inline-start" /> Add a login method
        </Button>
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add a login method</DialogTitle>
          <DialogDescription>
            Register an OAuth application at the provider with this callback
            URL, then paste its client id and secret:{" "}
            <code className="text-xs">{callback}</code>
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            add.mutate();
          }}
        >
          <div className="grid gap-2">
            <Label>Provider</Label>
            <Select value={type} onValueChange={setType}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {kinds.map((k) => (
                  <SelectItem key={k} value={k}>
                    {kindLabels[k] ?? k}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {type === "oidc" && (
            <div className="grid gap-2">
              <Label htmlFor="issuer">Issuer URL</Label>
              <Input
                id="issuer"
                placeholder="https://acme.okta.com"
                value={form.issuer}
                onChange={set("issuer")}
                required
              />
            </div>
          )}
          <div className="grid gap-2 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="clientId">Client id</Label>
              <Input
                id="clientId"
                value={form.clientId}
                onChange={set("clientId")}
                required
                autoComplete="off"
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="clientSecret">Client secret</Label>
              <Input
                id="clientSecret"
                type="password"
                value={form.clientSecret}
                onChange={set("clientSecret")}
                required
                autoComplete="off"
              />
            </div>
          </div>
          {type === "google" && (
            <div className="grid gap-2">
              <Label htmlFor="hostedDomain">
                Only accounts of this Google Workspace domain (optional)
              </Label>
              <Input
                id="hostedDomain"
                placeholder="acme.com"
                value={form.hostedDomain}
                onChange={set("hostedDomain")}
              />
            </div>
          )}
          {type === "microsoft" && (
            <div className="grid gap-2">
              <Label htmlFor="tenant">
                Only this Entra tenant (optional: id or domain)
              </Label>
              <Input
                id="tenant"
                placeholder="acme.com"
                value={form.tenant}
                onChange={set("tenant")}
              />
            </div>
          )}
          {type === "github" && (
            <div className="grid gap-2">
              <Label htmlFor="org">
                Only members of this organisation (optional; its teams become
                groups)
              </Label>
              <Input
                id="org"
                placeholder="acme"
                value={form.org}
                onChange={set("org")}
              />
            </div>
          )}
          <div className="grid gap-2 sm:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="cname">Button text (optional)</Label>
              <Input
                id="cname"
                placeholder={kindLabels[type]?.split(" ")[0]}
                value={form.name}
                onChange={set("name")}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="cid">Identifier (optional)</Label>
              <Input
                id="cid"
                placeholder={type}
                value={form.id}
                onChange={set("id")}
              />
            </div>
          </div>
          <DialogFooter>
            <Button
              type="submit"
              disabled={add.isPending || !form.clientId || !form.clientSecret}
            >
              Add
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

const policyHelp: Record<string, { label: string; text: string }> = {
  open: {
    label: "Anyone who can sign in",
    text: "Whoever signs in through one of the methods above becomes a person of this workspace (they still need a role to see or open anything).",
  },
  company: {
    label: "Only accounts of a claimed domain",
    text: "New people join only through the sign-in method of a verified domain below. People who already signed in keep their access.",
  },
  listed: {
    label: "Only people already in a team",
    text: "New people join only if an administrator listed their email in a team or granted them a role first.",
  },
};

function JoinPolicyCard() {
  const qc = useQueryClient();
  const ws = useQuery({ queryKey: ["workspace"], queryFn: api.workspace });
  const update = useMutation({
    mutationFn: (joinPolicy: string) => api.updateWorkspace({ joinPolicy }),
    onSuccess: () => {
      toast.success("Join policy saved");
      qc.invalidateQueries({ queryKey: ["workspace"] });
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const policy = ws.data?.joinPolicy ?? "open";
  return (
    <Card>
      <CardHeader>
        <CardTitle>Who may join</CardTitle>
        <CardDescription>
          What happens when someone signs in for the first time.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <Select
          value={policy}
          disabled={!ws.data || update.isPending}
          onValueChange={(v) => update.mutate(v)}
        >
          <SelectTrigger className="w-full sm:w-96">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {Object.entries(policyHelp).map(([k, v]) => (
              <SelectItem key={k} value={k}>
                {v.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p className="text-xs text-muted-foreground">
          {policyHelp[policy]?.text}
        </p>
      </CardContent>
    </Card>
  );
}

function DomainClaimsCard() {
  const qc = useQueryClient();
  const claims = useQuery({
    queryKey: ["domain-claims"],
    queryFn: api.domainClaims,
    retry: false,
  });
  const methods = useQuery({
    queryKey: ["login-methods"],
    queryFn: api.loginMethods,
    retry: false,
  });
  const [domain, setDomain] = useState("");
  const [connector, setConnector] = useState("");
  const refresh = () => qc.invalidateQueries({ queryKey: ["domain-claims"] });
  const claim = useMutation({
    mutationFn: () =>
      api.claimDomain(domain.trim(), connector === "any" ? "" : connector),
    onSuccess: () => {
      toast.success("Domain added; publish the TXT record, then verify");
      setDomain("");
      refresh();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const verify = useMutation({
    mutationFn: (d: DomainClaim) => api.verifyDomain(d.domain),
    onSuccess: (d) => {
      toast.success(`${d.domain} verified`);
      refresh();
    },
    onError: (e: Error) => toast.error(e.message),
  });
  const unclaim = useMutation({
    mutationFn: (d: DomainClaim) => api.unclaimDomain(d.domain),
    onSuccess: refresh,
    onError: (e: Error) => toast.error(e.message),
  });
  const connectors = methods.data?.connectors ?? [];

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheck className="size-4" /> Company domains
        </CardTitle>
        <CardDescription>
          Prove you own an email domain with a DNS record. Accounts of a
          verified domain count as the company&apos;s people and, when a method
          is chosen, must sign in through it — no company address behind a
          password someone made up.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        {claims.isLoading && <Skeleton className="h-12 w-full" />}
        {claims.data && claims.data.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Domain</TableHead>
                <TableHead>Sign in through</TableHead>
                <TableHead>DNS record</TableHead>
                <TableHead className="text-right">Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {claims.data.map((d) => (
                <TableRow key={d.domain}>
                  <TableCell className="font-mono text-xs">
                    {d.domain}
                  </TableCell>
                  <TableCell className="text-xs">
                    {d.connector || (
                      <span className="text-muted-foreground">any method</span>
                    )}
                  </TableCell>
                  <TableCell className="font-mono text-[11px] text-muted-foreground">
                    {d.record} TXT {d.recordValue}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex items-center justify-end gap-1">
                      {d.verified ? (
                        <Badge variant="secondary">verified</Badge>
                      ) : (
                        <Button
                          size="xs"
                          variant="outline"
                          disabled={verify.isPending}
                          onClick={() => verify.mutate(d)}
                        >
                          Verify
                        </Button>
                      )}
                      <Button
                        size="icon"
                        variant="ghost"
                        aria-label={`Remove ${d.domain}`}
                        onClick={() => unclaim.mutate(d)}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <form
          className="grid gap-2 sm:grid-cols-[1fr_auto_auto]"
          onSubmit={(e) => {
            e.preventDefault();
            if (domain.trim()) claim.mutate();
          }}
        >
          <Input
            placeholder="acme.com"
            value={domain}
            onChange={(e) => setDomain(e.target.value)}
            className="h-8 text-xs"
          />
          <Select value={connector || "any"} onValueChange={setConnector}>
            <SelectTrigger className="h-8 w-56 text-xs">
              <SelectValue placeholder="sign in through" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="any">any method</SelectItem>
              {connectors.map((c) => (
                <SelectItem key={c.id} value={c.id}>
                  {c.name} only
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Button
            type="submit"
            size="sm"
            disabled={!domain.trim() || claim.isPending}
          >
            <Plus data-icon="inline-start" /> Claim
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}
