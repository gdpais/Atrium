import { expect, test as base, type Page } from "@playwright/test";

export interface Account {
  email: string;
  password: string;
}

export interface Options {
  // Users made by scripts/kind-e2e.sh, with the viewer role only and with
  // the operator role only.
  viewer: Account;
  operator: Account;
}

export const test = base.extend<Options>({
  viewer: [{ email: "", password: "" }, { option: true }],
  operator: [{ email: "", password: "" }, { option: true }],
});

test.beforeEach(({ viewer, operator }) => {
  for (const [role, account] of Object.entries({ viewer, operator })) {
    if (!account.email || !account.password) {
      const name = role.toUpperCase();
      throw new Error(`set HEARTBEAT_E2E_${name}_EMAIL and HEARTBEAT_E2E_${name}_PASSWORD_FILE`);
    }
  }
});

export async function logIn(page: Page, account: Account) {
  await page.getByLabel("Email").fill(account.email);
  // A regular expression: Mantine appends a required mark to the label, and
  // "Toggle password visibility" also names a password.
  await page.getByLabel(/^Password/).fill(account.password);
  await page.getByRole("button", { name: "Log in" }).click();
}

export { expect };
