import { Badge } from "@mantine/core";
import type { Status } from "../api/inventory";

export function StatusBadge({ status }: { status: Status }) {
  return (
    <Badge color={status === "active" ? "green" : "gray"} variant="light">
      {status}
    </Badge>
  );
}
