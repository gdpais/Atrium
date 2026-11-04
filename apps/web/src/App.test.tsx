import { render, screen } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Providers, routes } from "./App";

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(
    <Providers>
      <RouterProvider router={router} />
    </Providers>,
  );
}

function answer(status: number, body?: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: Request) => {
      expect(new URL(input.url).pathname).toBe("/api/v1/session");
      return new Response(body === undefined ? null : JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("home", () => {
  it("names the signed-in user", async () => {
    answer(200, {
      id: "00000000-0000-0000-0000-000000000001",
      user: { id: "00000000-0000-0000-0000-000000000002", email: "ana@example.com", display_name: "Ana", roles: ["viewer"] },
      created_at: "2026-11-04T10:00:00Z",
      expires_at: "2026-11-04T22:00:00Z",
    });
    renderAt("/");
    expect(await screen.findByText(/Signed in as Ana \(ana@example\.com\)/)).toBeInTheDocument();
  });

  it("says so when there is no session", async () => {
    answer(401, { title: "Unauthorized", status: 401 });
    renderAt("/");
    expect(await screen.findByText("You are not signed in.")).toBeInTheDocument();
  });

  it("shows an error when the API fails", async () => {
    answer(500, { title: "Internal Server Error", status: 500 });
    renderAt("/");
    expect(await screen.findByText("The API could not be reached", {}, { timeout: 3000 })).toBeInTheDocument();
  });
});

describe("routing", () => {
  it("answers an unknown path with a not-found page", () => {
    answer(401);
    renderAt("/no/such/page");
    expect(screen.getByRole("heading", { name: "Page not found" })).toBeInTheDocument();
  });
});
