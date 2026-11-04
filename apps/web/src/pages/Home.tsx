import { Anchor, List, Text, Title } from "@mantine/core";
import { Link } from "react-router";
import { hasRole, useSession } from "../api/session";
import { sections } from "../nav";

export function Home() {
  const { data: session } = useSession();
  if (!session) return null;
  return (
    <>
      <Title order={1}>Heartbeat</Title>
      <Text mt="md">
        Signed in as {session.user.display_name || session.user.email} ({session.user.email}), with the{" "}
        {session.user.roles.join(", ")} {session.user.roles.length === 1 ? "role" : "roles"}.
      </Text>
      <List mt="md">
        {sections
          .filter((section) => hasRole(session, section.role))
          .map((section) => (
            <List.Item key={section.path}>
              <Anchor component={Link} to={section.path}>
                {section.label}
              </Anchor>
            </List.Item>
          ))}
      </List>
    </>
  );
}
