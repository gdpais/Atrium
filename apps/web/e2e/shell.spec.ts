import { expect, logIn, test } from "./fixtures";

// Slice 2 of the Web UI (TODO 5.1): login, the session, role-gated
// navigation and logout, in Chromium against the API on kind.

test("a wrong password is refused with a message", async ({ page, viewer }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/\/login$/);
  await logIn(page, { email: viewer.email, password: `${viewer.password}-wrong` });
  await expect(page.getByRole("alert")).toContainText("The email or password is wrong.");
  await expect(page).toHaveURL(/\/login$/);
});

test("a viewer logs in, sees the read-only navigation and logs out", async ({ page, context, viewer }) => {
  // A page opened signed out goes to the login page, and back after it.
  await page.goto("/environments");
  await expect(page).toHaveURL(/\/login\?next=%2Fenvironments$/);
  await logIn(page, viewer);
  await expect(page.getByRole("heading", { name: "Environments" })).toBeVisible();
  await expect(page).toHaveURL(/\/environments$/);

  // The session cookie is the API's, out of the page's reach.
  const cookie = (await context.cookies()).find((c) => c.name === "__Host-heartbeat_session");
  expect(cookie).toMatchObject({ httpOnly: true, secure: true, sameSite: "Lax", path: "/" });

  // Every section but users, which needs admin.
  const nav = page.getByRole("navigation", { name: "Sections" });
  await expect(nav.getByRole("link")).toHaveText(["Environments", "Applications", "Database targets", "Probe assignments"]);
  await page.goto("/users");
  await expect(page.getByRole("heading", { name: "Not allowed" })).toBeVisible();
  expect((await page.request.get("/api/v1/users")).status()).toBe(403);

  // A reload keeps the session; logging out ends it at the API too.
  await page.goto("/");
  await expect(page.getByText(`(${viewer.email}), with the viewer role`)).toBeVisible();
  await page.getByRole("button", { name: "Log out" }).click();
  await expect(page).toHaveURL(/\/login$/);
  expect((await page.request.get("/api/v1/session")).status()).toBe(401);
  await page.goto("/environments");
  await expect(page).toHaveURL(/\/login\?next=%2Fenvironments$/);
});
