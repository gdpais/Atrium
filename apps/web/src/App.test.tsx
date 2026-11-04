import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Providers, routes } from "./App";
import { session, stubAPI, unauthorized } from "./test/api";

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(
    <Providers>
      <RouterProvider router={router} />
    </Providers>,
  );
  return router;
}

function where(router: ReturnType<typeof renderAt>) {
  const { pathname, search } = router.state.location;
  return pathname + search;
}

async function logIn(email: string, password: string) {
  const user = userEvent.setup();
  await user.type(await screen.findByLabelText(/Email/), email);
  await user.type(screen.getByLabelText(/Password/), password);
  await user.click(screen.getByRole("button", { name: "Log in" }));
}

function navLinks() {
  const nav = screen.getByRole("navigation", { name: "Sections" });
  return within(nav)
    .getAllByRole("link")
    .map((link) => link.textContent);
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("login", () => {
  it("sends a signed-out user to the login page and back where they were", async () => {
    let signedIn = false;
    const calls = stubAPI({
      "GET /api/v1/session": () => (signedIn ? { status: 200, body: session(["viewer"]) } : unauthorized),
      "POST /api/v1/session": () => {
        signedIn = true;
        return { status: 201, body: session(["viewer"]) };
      },
    });
    const router = renderAt("/environments?view=all");
    await screen.findByRole("heading", { name: "Log in to Heartbeat" });
    expect(where(router)).toBe("/login?next=%2Fenvironments%3Fview%3Dall");

    await logIn("ana@example.com", "correct horse battery staple");
    expect(await screen.findByRole("heading", { name: "Environments" })).toBeInTheDocument();
    expect(where(router)).toBe("/environments?view=all");
    expect(calls.find((call) => call.method === "POST")?.body).toEqual({
      email: "ana@example.com",
      password: "correct horse battery staple",
    });
  });

  it("says the email or password is wrong on a 401", async () => {
    stubAPI({ "GET /api/v1/session": unauthorized, "POST /api/v1/session": unauthorized });
    renderAt("/login");
    await logIn("ana@example.com", "wrong password here");
    expect(await screen.findByRole("alert")).toHaveTextContent("The email or password is wrong.");
  });

  it("says when to try again on a 429", async () => {
    stubAPI({
      "GET /api/v1/session": unauthorized,
      "POST /api/v1/session": {
        status: 429,
        headers: { "Retry-After": "60" },
        body: { type: "about:blank", title: "Too Many Requests", status: 429 },
      },
    });
    renderAt("/login");
    await logIn("ana@example.com", "wrong password here");
    expect(await screen.findByRole("alert")).toHaveTextContent("Too many failed attempts. Try again in 60 seconds.");
  });

  it("ignores a next that leaves the app", async () => {
    stubAPI({ "GET /api/v1/session": { status: 200, body: session(["viewer"]) } });
    const router = renderAt("/login?next=%2F%2Fevil.example%2Fphish");
    await screen.findByRole("heading", { name: "Heartbeat" });
    expect(where(router)).toBe("/");
  });
});

describe("navigation", () => {
  it("shows a viewer every section but users", async () => {
    stubAPI({ "GET /api/v1/session": { status: 200, body: session(["viewer"]) } });
    renderAt("/");
    expect(await screen.findByText(/Signed in as Ana \(ana@example\.com\), with the viewer role/)).toBeInTheDocument();
    expect(navLinks()).toEqual(["Environments", "Applications", "Database targets", "Probe assignments"]);
  });

  it("shows an admin the users section", async () => {
    stubAPI({ "GET /api/v1/session": { status: 200, body: session(["viewer", "admin"], "Rui") } });
    renderAt("/");
    await screen.findByText(/Signed in as Rui/);
    expect(navLinks()).toContain("Users");
  });

  it("tells a viewer who opens an admin page that it is not allowed", async () => {
    stubAPI({ "GET /api/v1/session": { status: 200, body: session(["viewer"]) } });
    renderAt("/users");
    expect(await screen.findByRole("heading", { name: "Not allowed" })).toBeInTheDocument();
    expect(screen.getByText(/needs the admin role/)).toBeInTheDocument();
  });

  it("answers an unknown path with a not-found page", async () => {
    stubAPI({ "GET /api/v1/session": { status: 200, body: session(["viewer"]) } });
    renderAt("/no/such/page");
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument();
  });

  it("shows an error when the API fails", async () => {
    stubAPI({ "GET /api/v1/session": { status: 500, body: { type: "about:blank", title: "Internal Server Error", status: 500 } } });
    renderAt("/");
    expect(await screen.findByText("The API could not be reached", {}, { timeout: 3000 })).toBeInTheDocument();
  });
});

describe("logout", () => {
  it("ends the session and shows the login page", async () => {
    let signedIn = true;
    const calls = stubAPI({
      "GET /api/v1/session": () => (signedIn ? { status: 200, body: session(["viewer"]) } : unauthorized),
      "DELETE /api/v1/session": () => {
        signedIn = false;
        return { status: 204 };
      },
    });
    const router = renderAt("/environments");
    await userEvent.setup().click(await screen.findByRole("button", { name: "Log out" }));
    await screen.findByRole("heading", { name: "Log in to Heartbeat" });
    expect(where(router)).toBe("/login");
    expect(calls.some((call) => call.method === "DELETE" && call.path === "/api/v1/session")).toBe(true);
  });

  it("treats a session that already ended as logged out", async () => {
    stubAPI({
      "GET /api/v1/session": { status: 200, body: session(["viewer"]) },
      "DELETE /api/v1/session": unauthorized,
    });
    const router = renderAt("/");
    await userEvent.setup().click(await screen.findByRole("button", { name: "Log out" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
  });
});
