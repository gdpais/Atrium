import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "./client";
import { unwrap } from "./errors";
import type { components } from "./schema.gen";

type Schemas = components["schemas"];
export type Environment = Schemas["Environment"];
export type NewEnvironment = Schemas["NewEnvironment"];
export type EnvironmentUpdate = Schemas["EnvironmentUpdate"];
export type EnvironmentType = Schemas["EnvironmentType"];
export type Application = Schemas["Application"];
export type NewApplication = Schemas["NewApplication"];
export type ApplicationUpdate = Schemas["ApplicationUpdate"];
export type Criticality = Schemas["Criticality"];
export type Status = Schemas["Status"];

export const environmentTypes: EnvironmentType[] = ["production", "staging", "development", "test"];
export const criticalities: Criticality[] = ["low", "medium", "high", "critical"];
export const statuses: Status[] = ["active", "disabled"];

const environmentsKey = ["environments"] as const;
const applicationsKey = ["applications"] as const;

// Every environment, by name: the API does not page them (openapi.yaml).
export function useEnvironments() {
  return useQuery({
    queryKey: environmentsKey,
    queryFn: async () => unwrap(await api.GET("/api/v1/environments")).items,
  });
}

export function useCreateEnvironment() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (body: NewEnvironment) => unwrap(await api.POST("/api/v1/environments", { body })),
    onSuccess: () => client.invalidateQueries({ queryKey: environmentsKey }),
  });
}

export function useUpdateEnvironment() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async ({ id, body }: { id: string; body: EnvironmentUpdate }) =>
      unwrap(await api.PATCH("/api/v1/environments/{id}", { params: { path: { id } }, body })),
    onSuccess: () => client.invalidateQueries({ queryKey: environmentsKey }),
  });
}

export interface ApplicationFilter {
  environment_id?: string;
  status?: Status;
}

// Applications by name, a page at a time: the API pages them, so the list
// grows with "Load more" from each page's next_cursor.
export function useApplications(filter: ApplicationFilter) {
  return useInfiniteQuery({
    queryKey: [...applicationsKey, filter],
    initialPageParam: undefined as string | undefined,
    queryFn: async ({ pageParam }) =>
      unwrap(
        await api.GET("/api/v1/applications", {
          params: { query: { ...filter, ...(pageParam ? { cursor: pageParam } : {}) } },
        }),
      ),
    getNextPageParam: (page) => page.next_cursor,
  });
}

export function useCreateApplication() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async (body: NewApplication) => unwrap(await api.POST("/api/v1/applications", { body })),
    onSuccess: () => client.invalidateQueries({ queryKey: applicationsKey }),
  });
}

export function useUpdateApplication() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: async ({ id, body }: { id: string; body: ApplicationUpdate }) =>
      unwrap(await api.PATCH("/api/v1/applications/{id}", { params: { path: { id } }, body })),
    onSuccess: () => client.invalidateQueries({ queryKey: applicationsKey }),
  });
}

// changes returns the fields of next that differ from current, so a PATCH
// carries only what the user changed (the API keeps the rest).
export function changes<T extends object>(current: Partial<T>, next: T): Partial<T> {
  const out: Partial<T> = {};
  for (const key of Object.keys(next) as (keyof T)[]) {
    if (next[key] !== current[key]) out[key] = next[key];
  }
  return out;
}
