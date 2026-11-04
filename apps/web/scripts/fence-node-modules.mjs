// Go's ./... patterns walk every directory of the module, node_modules
// included, and some npm packages ship Go code (flatted does). A go.mod makes
// node_modules a module of its own, which ./... skips, so go test, vet and
// govulncheck never build what npm installed.
import { writeFileSync } from "node:fs";

writeFileSync(
  new URL("../node_modules/go.mod", import.meta.url),
  "// Written by apps/web/scripts/fence-node-modules.mjs: keeps Go's ./... out of node_modules.\nmodule heartbeat/apps/web/node_modules\n",
);
