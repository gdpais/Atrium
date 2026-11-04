import { expect, test as base, type Page } from "@playwright/test";

export interface Account {
  email: string;
  password: string;
}

export interface Options {
  // A user with the viewer role only, made by scripts/kind-e2e.sh.
  viewer: Account;
}

export const test = base.extend<Options>({
  viewer: [{ email: "", password: "" }, { option: true }],
});

test.beforeEach(({ viewer }) => {
  if (!viewer.email || !viewer.password) {
    throw new Error("set HEARTBEAT_E2E_VIEWER_EMAIL and HEARTBEAT_E2E_VIEWER_PASSWORD_FILE");
  }
});

export async function logIn(page: Page, account: Account) {
  await page.getByLabel("Email").fill(account.email);
  await page.getByLabel("Password", { exact: true }).fill(account.password);
  await page.getByRole("button", { name: "Log in" }).click();
}

export { expect };
