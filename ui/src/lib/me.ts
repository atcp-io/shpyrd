import { useQuery } from "@tanstack/react-query";
import { api, type Identity } from "./api";

// Roles and actions mirror pkg/authz: the API enforces, the UI hides.
export type Action =
  | "project.view"
  | "project.deploy"
  | "project.scale"
  | "project.config"
  | "project.exec"
  | "project.resource"
  | "project.members"
  | "project.destroy"
  | "cluster.view"
  | "cluster.admin"
  | "cluster.create";

const roleActions: Record<string, Action[]> = {
  user: [], // opens the app; nothing in this dashboard
  viewer: ["project.view"],
  developer: [
    "project.view",
    "project.deploy",
    "project.scale",
    "project.config",
    "project.exec",
  ],
  admin: [
    "project.view",
    "project.deploy",
    "project.scale",
    "project.config",
    "project.exec",
    "project.resource",
    "project.members",
    "project.destroy",
  ],
  "platform-viewer": ["cluster.view", "project.view"],
  "platform-admin": [
    "cluster.view",
    "cluster.admin",
    "cluster.create",
    "project.view",
    "project.deploy",
    "project.scale",
    "project.config",
    "project.exec",
    "project.resource",
    "project.members",
    "project.destroy",
  ],
};

export function can(
  me: Identity | undefined,
  action: Action,
  project?: string,
): boolean {
  // Before /api/me answers, or when auth is off, assume the page may show everything.
  if (!me) return true;
  const platform = me.roles?.platform;
  if (platform && roleActions[platform]?.includes(action)) return true;
  if (!project) return false;
  const role = me.roles?.projects?.[project];
  return !!role && (roleActions[role] ?? []).includes(action);
}

export function useMe() {
  return useQuery({
    queryKey: ["me"],
    queryFn: api.me,
    staleTime: 60_000,
    retry: false,
  });
}

// usePerms returns the checks a project page needs.
export function usePerms(project?: string) {
  const me = useMe();
  const check = (a: Action) => can(me.data, a, project);
  return {
    me: me.data,
    loaded: !!me.data,
    view: check("project.view"),
    deploy: check("project.deploy"),
    scale: check("project.scale"),
    config: check("project.config"),
    exec: check("project.exec"),
    resource: check("project.resource"),
    members: check("project.members"),
    destroy: check("project.destroy"),
    clusterView: check("cluster.view"),
    clusterAdmin: check("cluster.admin"),
    create: check("cluster.create"),
    role:
      me.data?.roles?.platform === "platform-admin"
        ? "admin"
        : (me.data?.roles?.projects?.[project ?? ""] ??
          (me.data?.roles?.platform === "platform-viewer"
            ? "viewer"
            : undefined)),
    enforced: me.data?.roles?.enforced ?? true,
  };
}
