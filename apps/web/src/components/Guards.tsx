import { Center, Container, Loader, Text, Title } from "@mantine/core";
import { Navigate, Outlet, useLocation } from "react-router";
import { hasRole, useSession, type Role } from "../api/session";
import { ErrorAlert } from "./ErrorAlert";

// RequireSession shows its routes to a signed-in user, and sends anyone else
// to the login page, which brings them back here afterwards.
export function RequireSession() {
  const session = useSession();
  const location = useLocation();
  if (session.isPending) {
    return (
      <Center h="100vh">
        <Loader aria-label="Loading" />
      </Center>
    );
  }
  if (session.isError) {
    return (
      <Container size="sm" py="xl">
        <ErrorAlert error={session.error} title="The API could not be reached" />
      </Container>
    );
  }
  if (!session.data) {
    const here = location.pathname + location.search + location.hash;
    const to = here === "/" ? "/login" : `/login?next=${encodeURIComponent(here)}`;
    return <Navigate to={to} replace />;
  }
  return <Outlet />;
}

// RequireRole shows its routes to a user with the role, or a better one, and
// says why to anyone else. The API refuses them all the same (403).
export function RequireRole({ role }: { role: Role }) {
  const { data } = useSession();
  if (!hasRole(data, role)) return <Forbidden role={role} />;
  return <Outlet />;
}

export function Forbidden({ role }: { role: Role }) {
  return (
    <Container size="sm" py="xl">
      <Title order={1}>Not allowed</Title>
      <Text mt="md">This page needs the {role} role. Ask an admin if you need it.</Text>
    </Container>
  );
}
