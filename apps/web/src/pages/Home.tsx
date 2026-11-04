import { Anchor, List, Text, Title } from "@mantine/core";
import { Link } from "react-router";
import { hasRole, useSession } from "../api/session";
import { sections } from "../nav";

export function Home() {
  const { data: session } = useSession();
  if (!session) return null;
  const { display_name, email, roles } = session.user;
  const who = display_name ? `${display_name} (${email})` : email;
  return (
    <>
      <Title order={1}>Heartbeat</Title>
      <Text mt="md">
        Signed in as {who}, with the {roles.join(", ")} {roles.length === 1 ? "role" : "roles"}.
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
