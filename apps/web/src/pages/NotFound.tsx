import { Anchor, Text, Title } from "@mantine/core";
import { Link } from "react-router";

export function NotFound() {
  return (
    <>
      <Title order={1}>Page not found</Title>
      <Text mt="md">
        <Anchor component={Link} to="/">
          Back to Heartbeat
        </Anchor>
      </Text>
    </>
  );
}
