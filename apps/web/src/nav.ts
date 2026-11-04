import type { Role } from "./api/session";

export interface Section {
  path: string;
  label: string;
  // The least role that may open it; the navigation hides it from others.
  role: Role;
}

// The app's sections, in the order the navigation lists them. Viewers read
// everything but users; admins alone manage users (openapi.yaml, tag users).
export const sections: Section[] = [
  { path: "/environments", label: "Environments", role: "viewer" },
  { path: "/applications", label: "Applications", role: "viewer" },
  { path: "/database-targets", label: "Database targets", role: "viewer" },
  { path: "/probe-assignments", label: "Probe assignments", role: "viewer" },
  { path: "/users", label: "Users", role: "admin" },
];
