import { Button, Group, Loader, Modal, NativeSelect, Stack, Table, Text, TextInput, Title } from "@mantine/core";
import { useState, type FormEvent } from "react";
import { useSearchParams } from "react-router";
import {
  changes,
  criticalities,
  statuses,
  useApplications,
  useCreateApplication,
  useEnvironments,
  useUpdateApplication,
  type Application,
  type Criticality,
  type Environment,
  type Status,
} from "../api/inventory";
import { hasRole, useSession } from "../api/session";
import { ErrorAlert } from "../components/ErrorAlert";
import { StatusBadge } from "../components/StatusBadge";

export function Applications() {
  const { data: session } = useSession();
  const canChange = hasRole(session, "operator");
  const [params, setParams] = useSearchParams();
  const environmentId = params.get("environment") ?? "";
  // Only a status the API knows: anything else in the address lists all.
  const status = statuses.find((known) => known === params.get("status")) ?? "";
  const environments = useEnvironments();
  const applications = useApplications({
    ...(environmentId ? { environment_id: environmentId } : {}),
    ...(status ? { status } : {}),
  });
  const [editing, setEditing] = useState<Application | "new" | null>(null);

  const byId = new Map(environments.data?.map((environment) => [environment.id, environment]));
  // New applications go only to active environments (ApplicationForm).
  const noActive = environments.isSuccess && !environments.data.some((environment) => environment.status === "active");
  // An environment in the address that is not listed (a link to one since
  // deleted) still shows as the filter, so the select does not read "All".
  const environmentChoices = (environments.data ?? []).map((e) => ({ value: e.id, label: e.name }));
  if (environmentId && environments.isSuccess && !byId.has(environmentId)) {
    environmentChoices.push({ value: environmentId, label: "Unknown environment" });
  }
  const items = applications.data?.pages.flatMap((page) => page.items) ?? [];
  const filter = (key: string, value: string) =>
    setParams((current) => {
      const next = new URLSearchParams(current);
      if (value) next.set(key, value);
      else next.delete(key);
      return next;
    });

  return (
    <>
      <Group justify="space-between">
        <Title order={1}>Applications</Title>
        {canChange && (
          <Button onClick={() => setEditing("new")} disabled={!environments.isSuccess || noActive}>
            New application
          </Button>
        )}
      </Group>
      <Group mt="md" align="flex-end">
        <NativeSelect
          label="Environment"
          value={environmentId}
          onChange={(event) => filter("environment", event.currentTarget.value)}
          data={[{ value: "", label: "All environments" }, ...environmentChoices]}
        />
        <NativeSelect
          label="Status"
          value={status}
          onChange={(event) => filter("status", event.currentTarget.value)}
          data={[{ value: "", label: "Any status" }, ...statuses]}
        />
      </Group>
      {environments.isError && <ErrorAlert error={environments.error} />}
      {canChange && noActive && (
        <Text mt="md" c="dimmed">
          New applications need an active environment.
        </Text>
      )}
      {applications.isPending && <Loader mt="md" aria-label="Loading" />}
      {applications.isError && <ErrorAlert error={applications.error} />}
      {applications.isSuccess && items.length === 0 && (
        <Text mt="md" c="dimmed">
          No applications{environmentId || status ? " match" : " yet"}.
        </Text>
      )}
      {items.length > 0 && (
        <Table.ScrollContainer minWidth={720} mt="md">
          <Table striped highlightOnHover aria-label="Applications">
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Name</Table.Th>
                <Table.Th>Environment</Table.Th>
                <Table.Th>Platform</Table.Th>
                <Table.Th>Owner team</Table.Th>
                <Table.Th>Criticality</Table.Th>
                <Table.Th>Status</Table.Th>
                {canChange && <Table.Th aria-label="Actions" />}
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {items.map((application) => (
                <Table.Tr key={application.id}>
                  <Table.Td>{application.name}</Table.Td>
                  <Table.Td>{byId.get(application.environment_id)?.name ?? "…"}</Table.Td>
                  <Table.Td>{application.platform}</Table.Td>
                  <Table.Td>{application.owner_team}</Table.Td>
                  <Table.Td>{application.criticality}</Table.Td>
                  <Table.Td>
                    <StatusBadge status={application.status} />
                  </Table.Td>
                  {canChange && (
                    <Table.Td>
                      <Button
                        variant="subtle"
                        size="xs"
                        // The form needs the environments to name this one.
                        disabled={!environments.isSuccess}
                        onClick={() => setEditing(application)}
                        aria-label={`Edit ${application.name}`}
                      >
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
      {applications.hasNextPage && (
        <Button variant="default" mt="md" loading={applications.isFetchingNextPage} onClick={() => applications.fetchNextPage()}>
          Load more
        </Button>
      )}
      {editing && environments.data && (
        <ApplicationForm
          application={editing === "new" ? undefined : editing}
          environments={environments.data}
          defaultEnvironment={environmentId}
          onClose={() => setEditing(null)}
        />
      )}
    </>
  );
}

// The platform as the API checks it (openapi.yaml, Platform).
const platformPattern = /^[a-z][a-z0-9_]{0,62}$/;

function ApplicationForm({
  application,
  environments,
  defaultEnvironment,
  onClose,
}: {
  application: Application | undefined;
  environments: Environment[];
  defaultEnvironment: string;
  onClose: () => void;
}) {
  const create = useCreateApplication();
  const update = useUpdateApplication();
  const mutation = application ? update : create;
  const active = environments.filter((environment) => environment.status === "active");
  const [environmentId, setEnvironmentId] = useState(
    application?.environment_id ?? (active.some((e) => e.id === defaultEnvironment) ? defaultEnvironment : (active[0]?.id ?? "")),
  );
  const [name, setName] = useState(application?.name ?? "");
  const [platform, setPlatform] = useState(application?.platform ?? "outsystems_traditional");
  const [ownerTeam, setOwnerTeam] = useState(application?.owner_team ?? "");
  const [criticality, setCriticality] = useState<Criticality>(application?.criticality ?? "medium");
  const [status, setStatus] = useState<Status>(application?.status ?? "active");

  const platformError = platform !== "" && !platformPattern.test(platform) ? "Lower-case letters, digits and underscores, starting with a letter" : undefined;
  const valid = name.trim() !== "" && platform !== "" && !platformError && environmentId !== "";

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const fields = { name: name.trim(), platform, owner_team: ownerTeam.trim(), criticality, status };
    if (application) {
      update.mutate({ id: application.id, body: changes(application, fields) }, { onSuccess: onClose });
    } else {
      create.mutate({ ...fields, environment_id: environmentId }, { onSuccess: onClose });
    }
  };

  // An application's environment is fixed once it is created, so editing
  // shows it, whatever its status, and new ones go only to active ones.
  const choices = application ? environments.filter((environment) => environment.id === application.environment_id) : active;

  return (
    <Modal opened onClose={onClose} title={application ? `Edit ${application.name}` : "New application"}>
      <form onSubmit={submit} noValidate>
        <Stack>
          {mutation.isError && <ErrorAlert error={mutation.error} title="Not saved" />}
          <NativeSelect
            label="Environment"
            required
            disabled={application !== undefined}
            description={application ? "Fixed once created" : undefined}
            value={environmentId}
            onChange={(event) => setEnvironmentId(event.currentTarget.value)}
            data={choices.map((environment) => ({ value: environment.id, label: environment.name }))}
          />
          <TextInput label="Name" required value={name} onChange={(event) => setName(event.currentTarget.value)} maxLength={200} data-autofocus />
          <TextInput
            label="Platform"
            required
            description="Such as outsystems_traditional, outsystems_reactive or dotnet"
            value={platform}
            error={platformError}
            onChange={(event) => setPlatform(event.currentTarget.value)}
            maxLength={63}
          />
          <TextInput label="Owner team" value={ownerTeam} onChange={(event) => setOwnerTeam(event.currentTarget.value)} maxLength={200} />
          <NativeSelect label="Criticality" data={criticalities} value={criticality} onChange={(event) => setCriticality(event.currentTarget.value as Criticality)} />
          <NativeSelect label="Status" data={statuses} value={status} onChange={(event) => setStatus(event.currentTarget.value as Status)} />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" loading={mutation.isPending} disabled={!valid}>
              {application ? "Save" : "Create"}
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
