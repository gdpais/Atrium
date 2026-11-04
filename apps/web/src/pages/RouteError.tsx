import { Anchor, Container, Text, Title } from "@mantine/core";
import { Link, useRouteError } from "react-router";
import { ErrorAlert } from "../components/ErrorAlert";

// RouteError replaces a page that failed to render, so one broken screen
// says what happened instead of leaving the app blank.
export function RouteError() {
  const error = useRouteError();
  return (
    <Container size="sm" py="xl">
      <Title order={1}>This page failed</Title>
      <ErrorAlert error={error} />
      <Text mt="md">
        <Anchor component={Link} to="/" reloadDocument>
          Back to Heartbeat
        </Anchor>
      </Text>
    </Container>
  );
}
