import { Anchor, Button, Code, Group, Loader, Modal, NativeSelect, Stack, Table, Text, Textarea, TextInput, Title } from "@mantine/core";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";
import {
  changes,
  environmentTypes,
  statuses,
  useCreateEnvironment,
  useEnvironments,
  useUpdateEnvironment,
  type Environment,
  type EnvironmentType,
  type Status,
} from "../api/inventory";
import { hasRole, useSession } from "../api/session";
import { ErrorAlert } from "../components/ErrorAlert";
import { StatusBadge } from "../components/StatusBadge";

export function Environments() {
  const { data: session } = useSession();
  const canChange = hasRole(session, "operator");
  const environments = useEnvironments();
  const [editing, setEditing] = useState<Environment | "new" | null>(null);

  return (
    <>
      <Group justify="space-between">
        <Title order={1}>Environments</Title>
        {canChange && <Button onClick={() => setEditing("new")}>New environment</Button>}
      </Group>
      {environments.isPending && <Loader mt="md" aria-label="Loading" />}
      {environments.isError && <ErrorAlert error={environments.error} />}
      {environments.data?.length === 0 && (
        <Text mt="md" c="dimmed">
          No environments yet.
        </Text>
      )}
      {!!environments.data?.length && (
        <Table.ScrollContainer minWidth={640} mt="md">
          <Table striped highlightOnHover aria-label="Environments">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Slug</Table.Th>
                <Table.Th>Type</Table.Th>
                <Table.Th>Status</Table.Th>
                <Table.Th>Description</Table.Th>
                {canChange && <Table.Th aria-label="Actions" />}
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {environments.data.map((environment) => (
                <Table.Tr key={environment.id}>
                  <Table.Td>
                    <Anchor component={Link} to={`/applications?environment=${environment.id}`}>
                      {environment.name}
                    </Anchor>
                  </Table.Td>
                  <Table.Td>
                    <Code>{environment.slug}</Code>
                  </Table.Td>
                  <Table.Td>{environment.type}</Table.Td>
                  <Table.Td>
                    <StatusBadge status={environment.status} />
                  </Table.Td>
                  <Table.Td>{environment.description}</Table.Td>
                  {canChange && (
                    <Table.Td>
                      <Button variant="subtle" size="xs" onClick={() => setEditing(environment)} aria-label={`Edit ${environment.name}`}>
                        Edit
                      </Button>
                    </Table.Td>
                  )}
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
      {editing && <EnvironmentForm environment={editing === "new" ? undefined : editing} onClose={() => setEditing(null)} />}
    </>
  );
}

// The slug as the API checks it (openapi.yaml, Slug).
const slugPattern = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

function EnvironmentForm({ environment, onClose }: { environment: Environment | undefined; onClose: () => void }) {
  const create = useCreateEnvironment();
  const update = useUpdateEnvironment();
  const mutation = environment ? update : create;
  const [name, setName] = useState(environment?.name ?? "");
  const [slug, setSlug] = useState(environment?.slug ?? "");
  const [type, setType] = useState<EnvironmentType>(environment?.type ?? "development");
  const [description, setDescription] = useState(environment?.description ?? "");
  const [status, setStatus] = useState<Status>(environment?.status ?? "active");

  const slugError = !environment && slug !== "" && !slugPattern.test(slug) ? "Lower-case letters, digits and hyphens; starts and ends with a letter or digit" : undefined;
  const valid = name.trim() !== "" && (environment !== undefined || (slug !== "" && !slugError));

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const fields = { name: name.trim(), type, description, status };
    if (environment) {
      update.mutate({ id: environment.id, body: changes(environment, fields) }, { onSuccess: onClose });
    } else {
      create.mutate({ ...fields, slug }, { onSuccess: onClose });
    }
  };

  return (
    <Modal opened onClose={onClose} title={environment ? `Edit ${environment.name}` : "New environment"}>
      <form onSubmit={submit} noValidate>
        <Stack>
          {mutation.isError && <ErrorAlert error={mutation.error} title="Not saved" />}
          <TextInput label="Name" required value={name} onChange={(event) => setName(event.currentTarget.value)} maxLength={200} data-autofocus />
          <TextInput
            label="Slug"
            required
            disabled={environment !== undefined}
            description="How the collectors and the telemetry name it; fixed once created"
            value={slug}
            error={slugError}
            onChange={(event) => setSlug(event.currentTarget.value)}
            maxLength={63}
          />
          <NativeSelect label="Type" data={environmentTypes} value={type} onChange={(event) => setType(event.currentTarget.value as EnvironmentType)} />
          <Textarea label="Description" value={description} onChange={(event) => setDescription(event.currentTarget.value)} maxLength={2000} autosize minRows={2} />
          <NativeSelect label="Status" data={statuses} value={status} onChange={(event) => setStatus(event.currentTarget.value as Status)} />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" loading={mutation.isPending} disabled={!valid}>
              {environment ? "Save" : "Create"}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
