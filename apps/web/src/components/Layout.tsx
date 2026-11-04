import { Anchor, AppShell, Badge, Burger, Button, Group, NavLink, Text } from "@mantine/core";
import { useDisclosure } from "@mantine/hooks";
import { NavLink as RouterLink, Outlet, useNavigate } from "react-router";
import { hasRole, useLogout, useSession } from "../api/session";
import { sections } from "../nav";
import { ErrorAlert } from "./ErrorAlert";

// Layout is the frame of every page a signed-in user sees: the header with
// who is signed in and a way out, and the navigation their roles allow.
export function Layout() {
  const [opened, { toggle, close }] = useDisclosure();
  const { data: session } = useSession();
  const logout = useLogout();
  const navigate = useNavigate();
  const user = session?.user;

  return (
    <AppShell header={{ height: 56 }} navbar={{ width: 240, breakpoint: "sm", collapsed: { mobile: !opened } }} padding="md">
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between" wrap="nowrap">
          <Group gap="sm" wrap="nowrap">
            <Burger opened={opened} onClick={toggle} hiddenFrom="sm" size="sm" aria-label="Navigation" />
            <Anchor component={RouterLink} to="/" c="inherit" underline="never" fw={700} size="lg">
              Heartbeat
            </Anchor>
          </Group>
          {user && (
            <Group gap="sm" wrap="nowrap">
              <Text size="sm" visibleFrom="xs" aria-label="Signed in as">
                {user.display_name || user.email}
              </Text>
              {user.roles.map((role) => (
                <Badge key={role} variant="light" visibleFrom="xs">
                  {role}
                </Badge>
              ))}
              <Button
                variant="default"
                size="xs"
                loading={logout.isPending}
                onClick={async () => {
                  // mutateAsync, not mutate's callbacks: logging out unmounts
                  // this layout, and a callback of an unmounted mutate is dropped.
                  try {
                    await logout.mutateAsync();
                    navigate("/login", { replace: true });
                  } catch {
                    // logout.error says why, above the page.
                  }
                }}
              >
                Log out
              </Button>
            </Group>
          )}
        </Group>
      </AppShell.Header>

      <AppShell.Navbar p="xs" aria-label="Sections">
        {sections
          .filter((section) => hasRole(session, section.role))
          .map((section) => (
            <NavLink key={section.path} component={RouterLink} to={section.path} label={section.label} onClick={close} />
          ))}
      </AppShell.Navbar>

      <AppShell.Main>
        {logout.isError && <ErrorAlert error={logout.error} title="Logging out failed" />}
        <Outlet />
      </AppShell.Main>
    </AppShell>
  );
}
