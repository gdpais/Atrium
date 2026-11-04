import { expect, logIn, test } from "./fixtures";

// Slice 3 of the Web UI (TODO 5.2, 5.3): environments and applications, in
// Chromium against the API on kind. Names carry a run suffix, since the
// cluster keeps what an earlier run created.

test("an operator creates an environment and an application in it, and sees the API's refusal", async ({ page, operator }) => {
  const slug = `ui-${Date.now().toString(36)}`;
  const environment = `UI ${slug}`;
  const application = `Shop ${slug}`;

  await page.goto("/environments");
  await logIn(page, operator);

  // A new environment.
  await page.getByRole("button", { name: "New environment" }).click();
  let dialog = page.getByRole("dialog", { name: "New environment" });
  await dialog.getByLabel(/^Name/).fill(environment);
  await dialog.getByLabel(/^Slug/).fill(slug);
  await dialog.getByLabel(/^Type/).selectOption("test");
  await dialog.getByRole("button", { name: "Create" }).click();
  await expect(dialog).toBeHidden();
  const environmentRow = page.getByRole("row").filter({ hasText: slug });
  await expect(environmentRow.getByRole("cell")).toHaveText([environment, slug, "test", "active", "", "Edit"]);

  // Its slug again: the API's 409, with its detail, in the form.
  await page.getByRole("button", { name: "New environment" }).click();
  dialog = page.getByRole("dialog", { name: "New environment" });
  await dialog.getByLabel(/^Name/).fill(`${environment} again`);
  await dialog.getByLabel(/^Slug/).fill(slug);
  await dialog.getByRole("button", { name: "Create" }).click();
  await expect(dialog.getByRole("alert")).toContainText("Not saved");
  await dialog.getByRole("button", { name: "Cancel" }).click();

  // Its applications, from its link: an application created there belongs
  // to it, and the list names it.
  await environmentRow.getByRole("link", { name: environment }).click();
  await expect(page).toHaveURL(/\/applications\?environment=/);
  await expect(page.getByText("No applications match.")).toBeVisible();
  await page.getByRole("button", { name: "New application" }).click();
  dialog = page.getByRole("dialog", { name: "New application" });
  await expect(dialog.getByLabel(/^Environment/)).toHaveValue(/.+/);
  await expect(dialog.getByLabel(/^Environment/).locator("option:checked")).toHaveText(environment);
  await dialog.getByLabel(/^Name/).fill(application);
  await dialog.getByLabel(/^Owner team/).fill("UI tests");
  await dialog.getByLabel(/^Criticality/).selectOption("high");
  await dialog.getByRole("button", { name: "Create" }).click();
  await expect(dialog).toBeHidden();
  const row = page.getByRole("row").filter({ hasText: application });
  await expect(row.getByRole("cell")).toHaveText([application, environment, "outsystems_traditional", "UI tests", "high", "active", "Edit"]);

  // An edit keeps its environment and changes what the form sent.
  await row.getByRole("button", { name: `Edit ${application}` }).click();
  dialog = page.getByRole("dialog", { name: `Edit ${application}` });
  await expect(dialog.getByLabel(/^Environment/)).toBeDisabled();
  await dialog.getByLabel(/^Status/).selectOption("disabled");
  await dialog.getByRole("button", { name: "Save" }).click();
  await expect(dialog).toBeHidden();
  await expect(row.getByRole("cell").nth(5)).toHaveText("disabled");
});

test("a viewer reads the inventory but has nothing to change it with", async ({ page, viewer }) => {
  await page.goto("/environments");
  await logIn(page, viewer);
  await expect(page.getByRole("heading", { name: "Environments" })).toBeVisible();
  await expect(page.getByRole("table", { name: "Environments" })).toBeVisible();
  await expect(page.getByRole("button", { name: "New environment" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Edit / })).toHaveCount(0);
  await page.getByRole("navigation", { name: "Sections" }).getByRole("link", { name: "Applications" }).click();
  await expect(page.getByRole("heading", { name: "Applications" })).toBeVisible();
  await expect(page.getByRole("button", { name: "New application" })).toHaveCount(0);
});
