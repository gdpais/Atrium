import createClient from "openapi-fetch";
import type { paths } from "./schema.gen";

// The API's typed client, generated from apps/api/openapi.yaml (ADR 0020).
// The UI is served from the API's origin, so paths are relative and the
// browser sends the session cookie with every request. The page's own origin
// is the base: the API's, or Vite's dev server, which proxies to it.
// fetch is looked up on each request, not captured once, so tests can stub it.
export const api = createClient<paths>({
  baseUrl: window.location.origin,
  fetch: (request) => globalThis.fetch(request),
});
