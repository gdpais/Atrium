import { describe, expect, it } from "vitest";
import { ApiError } from "./api/errors";
import { createQueryClient } from "./api/queryClient";
import { hasRole, sessionKey } from "./api/session";
import { safeNext } from "./next";
import { session } from "./test/api";

describe("hasRole", () => {
  it("ranks viewer < operator < admin", () => {
    expect(hasRole(session(["viewer"]), "viewer")).toBe(true);
    expect(hasRole(session(["viewer"]), "operator")).toBe(false);
    expect(hasRole(session(["operator"]), "viewer")).toBe(true);
    expect(hasRole(session(["operator"]), "admin")).toBe(false);
    expect(hasRole(session(["admin"]), "operator")).toBe(true);
    expect(hasRole(session(["viewer", "admin"]), "admin")).toBe(true);
  });

  it("allows nothing without a session", () => {
    expect(hasRole(null, "viewer")).toBe(false);
    expect(hasRole(undefined, "viewer")).toBe(false);
  });
});

describe("safeNext", () => {
  it.each([
    ["/environments", "/environments"],
    ["/applications/1?tab=sources#links", "/applications/1?tab=sources#links"],
    [null, "/"],
    ["", "/"],
    ["https://evil.example/", "/"],
    ["//evil.example/", "/"],
    ["/\\evil.example/", "/"],
    ["/\t/evil.example/", "/"],
    ["/login", "/"],
    ["/login?next=/users", "/"],
    ["environments", "/"],
  ])("%s → %s", (next, want) => {
    expect(safeNext(next)).toBe(want);
  });
});

describe("a 401 from any request", () => {
  it("signs the user out and forgets what was loaded", async () => {
    const client = createQueryClient();
    client.setQueryData(sessionKey, session(["viewer"]));
    client.setQueryData(["environments"], [{ id: "1" }]);
    const expired = new ApiError(new Response(null, { status: 401 }), { title: "Unauthorized", status: 401 });
    await expect(
      client.fetchQuery({
        queryKey: ["applications"],
        queryFn: () => Promise.reject(expired),
      }),
    ).rejects.toBe(expired);
    expect(client.getQueryData(sessionKey)).toBeNull();
    expect(client.getQueryData(["environments"])).toBeUndefined();
  });

  it("is not retried, nor is any other 4xx", async () => {
    const client = createQueryClient();
    let attempts = 0;
    await expect(
      client.fetchQuery({
        queryKey: ["users"],
        queryFn: () => {
          attempts++;
          return Promise.reject(new ApiError(new Response(null, { status: 403 })));
        },
      }),
    ).rejects.toBeInstanceOf(ApiError);
    expect(attempts).toBe(1);
  });

  it("leaves a 5xx retried once", async () => {
    const client = createQueryClient();
    let attempts = 0;
    await expect(
      client.fetchQuery({
        queryKey: ["users"],
        retryDelay: 0,
        queryFn: () => {
          attempts++;
          return Promise.reject(new ApiError(new Response(null, { status: 503 })));
        },
      }),
    ).rejects.toBeInstanceOf(ApiError);
    expect(attempts).toBe(2);
  });
});
