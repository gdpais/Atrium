import { vi } from "vitest";
import type { Session } from "../api/session";

export interface Call {
  method: string;
  path: string;
  body: unknown;
}

type Answer = { status: number; body?: unknown; headers?: Record<string, string> };
type Handler = Answer | ((call: Call) => Answer);

// stubAPI answers the app's requests from a table keyed by "METHOD /path",
// and records them. A request the table does not name fails the test.
export function stubAPI(table: Record<string, Handler>) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (request: Request) => {
      const url = new URL(request.url);
      const text = await request.text();
      const call: Call = { method: request.method, path: url.pathname, body: text ? JSON.parse(text) : undefined };
      calls.push(call);
      const handler = table[`${call.method} ${call.path}`];
      if (!handler) throw new Error(`unexpected request ${call.method} ${call.path}`);
      const answer = typeof handler === "function" ? handler(call) : handler;
      const problem = answer.status >= 400;
      return new Response(answer.body === undefined ? null : JSON.stringify(answer.body), {
        status: answer.status,
        headers: { "Content-Type": problem ? "application/problem+json" : "application/json", ...answer.headers },
      });
    }),
  );
  return calls;
}

export function session(roles: Session["user"]["roles"], name = "Ana"): Session {
  return {
    id: "00000000-0000-0000-0000-000000000001",
    user: { id: "00000000-0000-0000-0000-000000000002", email: `${name.toLowerCase()}@example.com`, display_name: name, roles },
    created_at: "2026-11-04T10:00:00Z",
    expires_at: "2026-11-04T22:00:00Z",
  };
}

export const unauthorized: Answer = { status: 401, body: { type: "about:blank", title: "Unauthorized", status: 401 } };
