import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Providers, routes } from "./App";
import type { Application, Environment } from "./api/inventory";
import type { Role } from "./api/session";
import { problem, session, stubAPI, type Call } from "./test/api";

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(
    <Providers>
      <RouterProvider router={router} />
    </Providers>,
  );
  return router;
}

function environment(id: string, name: string, slug: string, fields: Partial<Environment> = {}): Environment {
  return {
    id,
    name,
    slug,
    type: "development",
    description: "",
    status: "active",
    labels: {},
    created_at: "2026-11-04T10:00:00Z",
    updated_at: "2026-11-04T10:00:00Z",
    ...fields,
  };
}

function application(id: string, environmentId: string, name: string, fields: Partial<Application> = {}): Application {
  return {
    id,
    environment_id: environmentId,
    name,
    platform: "outsystems_traditional",
    owner_team: "",
    criticality: "medium",
    status: "active",
    created_at: "2026-11-04T10:00:00Z",
    updated_at: "2026-11-04T10:00:00Z",
    ...fields,
  };
}

const dev = environment("10000000-0000-0000-0000-000000000001", "Development", "dev");
const prod = environment("10000000-0000-0000-0000-000000000002", "Production", "prod", { type: "production" });

function signedIn(roles: Role[]) {
  return { "GET /api/v1/session": { status: 200, body: session(roles) } };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("environments", () => {
  it("lists them for a viewer, without changing them", async () => {
    stubAPI({ ...signedIn(["viewer"]), "GET /api/v1/environments": { status: 200, body: { items: [dev, prod] } } });
    renderAt("/environments");
    const table = await screen.findByRole("table", { name: "Environments" });
    expect(within(table).getAllByRole("row")).toHaveLength(3);
    expect(within(table).getByRole("link", { name: "Production" })).toHaveAttribute("href", `/applications?environment=${prod.id}`);
    expect(screen.queryByRole("button", { name: "New environment" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Edit/ })).not.toBeInTheDocument();
  });

  it("lets an operator create one", async () => {
    const items = [dev];
    const calls = stubAPI({
      ...signedIn(["operator"]),
      "GET /api/v1/environments": () => ({ status: 200, body: { items } }),
      "POST /api/v1/environments": ({ body }) => {
        const created = environment("10000000-0000-0000-0000-000000000003", "Staging", "stg", body as Partial<Environment>);
        items.push(created);
        return { status: 201, body: created };
      },
    });
    renderAt("/environments");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "New environment" }));
    const dialog = await screen.findByRole("dialog", { name: "New environment" });
    await user.type(within(dialog).getByLabelText(/^Name/), "Staging");
    await user.type(within(dialog).getByLabelText(/^Slug/), "stg");
    await user.selectOptions(within(dialog).getByLabelText(/^Type/), "staging");
    await user.click(within(dialog).getByRole("button", { name: "Create" }));

    expect(await screen.findByRole("link", { name: "Staging" })).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.find((call) => call.method === "POST")?.body).toEqual({
      name: "Staging",
      slug: "stg",
      type: "staging",
      description: "",
      status: "active",
    });
  });

  it("shows the API's conflict in the form", async () => {
    stubAPI({
      ...signedIn(["operator"]),
      "GET /api/v1/environments": { status: 200, body: { items: [dev] } },
      "POST /api/v1/environments": problem(409, "Conflict", "an environment already has the slug dev"),
    });
    renderAt("/environments");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "New environment" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText(/^Name/), "Dev again");
    await user.type(within(dialog).getByLabelText(/^Slug/), "dev");
    await user.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(await within(dialog).findByText("an environment already has the slug dev")).toBeInTheDocument();
    expect(within(dialog).getByText("Not saved")).toBeInTheDocument();
  });

  it("refuses a slug the API would refuse, before sending it", async () => {
    const calls = stubAPI({ ...signedIn(["operator"]), "GET /api/v1/environments": { status: 200, body: { items: [] } } });
    renderAt("/environments");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "New environment" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText(/^Name/), "Bad");
    await user.type(within(dialog).getByLabelText(/^Slug/), "Not-OK-");
    expect(within(dialog).getByText(/Lower-case letters, digits and hyphens/)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeDisabled();
    expect(calls.some((call) => call.method === "POST")).toBe(false);
  });

  it("sends only what changed, and keeps the slug fixed", async () => {
    const calls = stubAPI({
      ...signedIn(["operator"]),
      "GET /api/v1/environments": { status: 200, body: { items: [dev] } },
      "PATCH /api/v1/environments/10000000-0000-0000-0000-000000000001": ({ body }) => ({
        status: 200,
        body: { ...dev, ...(body as object) },
      }),
    });
    renderAt("/environments");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Edit Development" }));
    const dialog = await screen.findByRole("dialog", { name: "Edit Development" });
    expect(within(dialog).getByLabelText(/^Slug/)).toBeDisabled();
    await user.selectOptions(within(dialog).getByLabelText(/^Status/), "disabled");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(calls.some((call) => call.method === "PATCH")).toBe(true));
    expect(calls.find((call) => call.method === "PATCH")?.body).toEqual({ status: "disabled" });
  });
});

describe("applications", () => {
  const shop = application("20000000-0000-0000-0000-000000000001", dev.id, "Shop");
  const billing = application("20000000-0000-0000-0000-000000000002", prod.id, "Billing", { owner_team: "Payments" });

  it("names each application's environment, and pages with Load more", async () => {
    const calls = stubAPI({
      ...signedIn(["viewer"]),
      "GET /api/v1/environments": { status: 200, body: { items: [dev, prod] } },
      "GET /api/v1/applications": ({ query }) =>
        query.get("cursor") === "next"
          ? { status: 200, body: { items: [billing] } }
          : { status: 200, body: { items: [shop], next_cursor: "next" } },
    });
    renderAt("/applications");
    const table = await screen.findByRole("table", { name: "Applications" });
    expect(within(table).getByRole("row", { name: /Shop Development/ })).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole("button", { name: "Load more" }));
    expect(await within(table).findByRole("row", { name: /Billing Production/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
    expect(calls.filter((call) => call.path === "/api/v1/applications").map((call) => call.query.get("cursor"))).toEqual([null, "next"]);
    expect(screen.queryByRole("button", { name: "New application" })).not.toBeInTheDocument();
  });

  it("filters by the environment in the address", async () => {
    const calls = stubAPI({
      ...signedIn(["viewer"]),
      "GET /api/v1/environments": { status: 200, body: { items: [dev, prod] } },
      "GET /api/v1/applications": { status: 200, body: { items: [billing] } },
    });
    renderAt(`/applications?environment=${prod.id}`);
    await screen.findByRole("table", { name: "Applications" });
    expect(screen.getByLabelText("Environment")).toHaveValue(prod.id);
    const listed = calls.filter((call: Call) => call.path === "/api/v1/applications");
    expect(listed.at(-1)?.query.get("environment_id")).toBe(prod.id);
  });

  it("still shows a filter by an environment the list does not hold", async () => {
    stubAPI({
      ...signedIn(["viewer"]),
      "GET /api/v1/environments": { status: 200, body: { items: [dev] } },
      "GET /api/v1/applications": { status: 200, body: { items: [] } },
    });
    const gone = "10000000-0000-0000-0000-000000000009";
    renderAt(`/applications?environment=${gone}`);
    expect(await screen.findByText("No applications match.")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByLabelText("Environment")).toHaveValue(gone));
    expect(within(screen.getByLabelText("Environment")).getByRole("option", { selected: true })).toHaveTextContent("Unknown environment");
  });

  it("offers no new application while every environment is disabled", async () => {
    stubAPI({
      ...signedIn(["operator"]),
      "GET /api/v1/environments": { status: 200, body: { items: [{ ...dev, status: "disabled" }] } },
      "GET /api/v1/applications": { status: 200, body: { items: [] } },
    });
    renderAt("/applications");
    expect(await screen.findByText("New applications need an active environment.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "New application" })).toBeDisabled();
  });

  it("offers no edit until the environments are known", async () => {
    stubAPI({
      ...signedIn(["operator"]),
      "GET /api/v1/environments": problem(500, "Internal Server Error"),
      "GET /api/v1/applications": { status: 200, body: { items: [shop] } },
    });
    renderAt("/applications");
    expect(await screen.findByRole("button", { name: "Edit Shop" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "New application" })).toBeDisabled();
  });

  it("lets an operator create one in an environment, and shows the API's refusal", async () => {
    let refuse = true;
    const calls = stubAPI({
      ...signedIn(["operator"]),
      "GET /api/v1/environments": { status: 200, body: { items: [dev, prod] } },
      "GET /api/v1/applications": { status: 200, body: { items: [] } },
      "POST /api/v1/applications": ({ body }) => {
        if (refuse) {
          refuse = false;
          return problem(409, "Conflict", "the environment already has an application named Shop");
        }
        return { status: 201, body: application("20000000-0000-0000-0000-000000000003", prod.id, "Shop", body as Partial<Application>) };
      },
    });
    renderAt("/applications");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "New application" }));
    const dialog = await screen.findByRole("dialog", { name: "New application" });
    await user.selectOptions(within(dialog).getByLabelText(/^Environment/), prod.id);
    await user.type(within(dialog).getByLabelText(/^Name/), "Shop");
    await user.type(within(dialog).getByLabelText(/^Owner team/), "Retail");
    await user.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(await within(dialog).findByText("the environment already has an application named Shop")).toBeInTheDocument();

    await user.click(within(dialog).getByRole("button", { name: "Create" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(calls.filter((call) => call.method === "POST").at(-1)?.body).toEqual({
      environment_id: prod.id,
      name: "Shop",
      platform: "outsystems_traditional",
      owner_team: "Retail",
      criticality: "medium",
      status: "active",
    });
  });

  it("keeps an application's environment fixed when editing it", async () => {
    const calls = stubAPI({
      ...signedIn(["operator"]),
      "GET /api/v1/environments": { status: 200, body: { items: [dev, prod] } },
      "GET /api/v1/applications": { status: 200, body: { items: [billing] } },
      "PATCH /api/v1/applications/20000000-0000-0000-0000-000000000002": ({ body }) => ({
        status: 200,
        body: { ...billing, ...(body as object) },
      }),
    });
    renderAt("/applications");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Edit Billing" }));
    const dialog = await screen.findByRole("dialog", { name: "Edit Billing" });
    expect(within(dialog).getByLabelText(/^Environment/)).toBeDisabled();
    expect(within(dialog).getByLabelText(/^Environment/)).toHaveValue(prod.id);
    await user.selectOptions(within(dialog).getByLabelText(/^Criticality/), "critical");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(calls.some((call) => call.method === "PATCH")).toBe(true));
    expect(calls.find((call) => call.method === "PATCH")?.body).toEqual({ criticality: "critical" });
  });
});
