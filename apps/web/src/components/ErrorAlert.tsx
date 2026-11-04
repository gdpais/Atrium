import { Alert } from "@mantine/core";
import { ApiError } from "../api/errors";

// ErrorAlert shows why a request failed: the API's problem details when it
// sent them, and what a 403 means, since the UI shows what a user's roles
// allow and the API may still refuse.
export function ErrorAlert({ error, title }: { error: unknown; title?: string }) {
  if (error instanceof ApiError && error.status === 403) {
    return (
      <Alert color="orange" title="Not allowed" mt="md">
        Your roles do not allow this. {error.problem?.detail}
      </Alert>
    );
  }
  const message = error instanceof Error ? error.message : String(error);
  return (
    <Alert color="red" title={title ?? (error instanceof ApiError ? error.problem?.title : undefined) ?? "Something went wrong"} mt="md">
      {message}
    </Alert>
  );
}
