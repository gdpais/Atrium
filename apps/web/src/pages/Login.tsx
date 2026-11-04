import { Alert, Button, Center, Loader, Paper, PasswordInput, Stack, TextInput, Title } from "@mantine/core";
import { useState, type FormEvent } from "react";
import { Navigate, useSearchParams } from "react-router";
import { ApiError } from "../api/errors";
import { useLogin, useSession } from "../api/session";
import { safeNext } from "../next";

export function Login() {
  const session = useSession();
  const login = useLogin();
  const [params] = useSearchParams();
  const next = safeNext(params.get("next"));
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");

  if (session.isPending) {
    return (
      <Center h="100vh">
        <Loader aria-label="Loading" />
      </Center>
    );
  }
  // Signed in already, or just now: useLogin stores the new session.
  if (session.data) return <Navigate to={next} replace />;

  const submit = (event: FormEvent) => {
    event.preventDefault();
    login.mutate({ email, password });
  };

  return (
    <Center mih="100vh" p="md">
      <Paper withBorder shadow="sm" p="xl" w="100%" maw={380}>
        <form onSubmit={submit} noValidate>
          <Stack>
            <Title order={1} size="h2">
              Log in to Heartbeat
            </Title>
            {login.isError && <LoginError error={login.error} />}
            <TextInput
              label="Email"
              type="email"
              autoComplete="username"
              required
              value={email}
              onChange={(event) => setEmail(event.currentTarget.value)}
            />
            <PasswordInput
              label="Password"
              autoComplete="current-password"
              required
              value={password}
              onChange={(event) => setPassword(event.currentTarget.value)}
            />
            <Button type="submit" loading={login.isPending} disabled={!email || !password}>
              Log in
            </Button>
          </Stack>
        </form>
      </Paper>
    </Center>
  );
}

// What went wrong, in the user's terms. The API answers a wrong password, an
// unknown email and a disabled user alike, so this does too.
function LoginError({ error }: { error: Error }) {
  let message = error.message;
  if (error instanceof ApiError) {
    switch (error.status) {
      case 401:
        message = "The email or password is wrong.";
        break;
      case 429: {
        const seconds = Number(error.response.headers.get("Retry-After"));
        message = `Too many failed attempts. Try again in ${seconds > 0 ? `${seconds} seconds` : "a while"}.`;
        break;
      }
      case 403:
        message = "The API does not accept logins from this address. Open Heartbeat at the address it is configured for.";
        break;
      case 400:
        message = error.problem?.detail ?? "Enter an email and a password.";
        break;
    }
  }
  return (
    <Alert color="red" role="alert" title="Not logged in">
      {message}
    </Alert>
  );
}
