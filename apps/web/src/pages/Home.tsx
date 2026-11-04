import { Alert, Container, Loader, Text, Title } from "@mantine/core";
import { useSession } from "../api/session";

export function Home() {
  const session = useSession();
  return (
    <Container size="sm" py="xl">
      <Title order={1}>Heartbeat</Title>
      {session.isPending && <Loader size="sm" mt="md" aria-label="Loading" />}
      {session.isError && (
        <Alert color="red" mt="md" title="The API could not be reached">
          {session.error.message}
        </Alert>
      )}
      {session.data === null && <Text mt="md">You are not signed in.</Text>}
      {session.data && (
        <Text mt="md">
          Signed in as {session.data.user.display_name} ({session.data.user.email}).
        </Text>
      )}
    </Container>
  );
}
