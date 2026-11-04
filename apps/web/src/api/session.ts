import { useQuery } from "@tanstack/react-query";
import { api } from "./client";
import type { components } from "./schema.gen";

export type Session = components["schemas"]["Session"];

// The signed-in user's session, or null when there is none (401).
export function useSession() {
  return useQuery({
    queryKey: ["session"],
    queryFn: async (): Promise<Session | null> => {
      const { data, response } = await api.GET("/api/v1/session");
      if (response.status === 401) return null;
      if (!data) throw new Error(`the API answered ${response.status}`);
      return data;
    },
  });
}
