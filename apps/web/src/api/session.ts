import { useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { unwrap } from "./errors";
import type { components } from "./schema.gen";

export type Session = components["schemas"]["Session"];
export type Role = components["schemas"]["Role"];
export type Credentials = components["schemas"]["Credentials"];

export const sessionKey = ["session"] as const;

// Roles rank viewer < operator < admin, and a user may do what any of their
// roles allows (openapi.yaml). The API stays the authority: the UI only
// hides what a user's roles do not allow (ADR 0020 §4).
const rank: Record<Role, number> = { viewer: 0, operator: 1, admin: 2 };

export function hasRole(session: Session | null | undefined, least: Role): boolean {
  return session?.user.roles.some((role) => rank[role] >= rank[least]) ?? false;
}

// The signed-in user's session, or null when there is none (401).
export function useSession() {
  return useQuery({
    queryKey: sessionKey,
    queryFn: async (): Promise<Session | null> => {
      const result = await api.GET("/api/v1/session");
      if (result.response.status === 401) return null;
      return unwrap(result);
    },
    staleTime: 60_000,
  });
}

export function useLogin() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (credentials: Credentials) => unwrap(await api.POST("/api/v1/session", { body: credentials })),
    onSuccess: (session) => {
      // Whatever an earlier user had loaded is not this user's to see.
      client.removeQueries({ predicate: (query) => query.queryKey[0] !== sessionKey[0] });
      client.setQueryData(sessionKey, session);
    },
  });
}

export function useLogout() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const result = await api.DELETE("/api/v1/session");
      // A session that already ended is logged out all the same.
      if (result.response.status !== 401) unwrap(result);
    },
    onSuccess: () => signedOut(client),
  });
}

// signedOut forgets everything the API answered for the session that ended,
// and records that there is none, so the app shows the login page.
export function signedOut(client: QueryClient) {
  client.removeQueries({ predicate: (query) => query.queryKey[0] !== sessionKey[0] });
  client.setQueryData(sessionKey, null);
}
