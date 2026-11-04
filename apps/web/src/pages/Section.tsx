import { Text, Title } from "@mantine/core";

// Section stands in for a screen a later slice of TODO 5.2 builds, so the
// navigation and its role gates work end to end before the screens exist.
export function Section({ title }: { title: string }) {
  return (
    <>
      <Title order={1}>{title}</Title>
      <Text mt="md" c="dimmed">
        This screen is not built yet.
      </Text>
    </>
  );
}
