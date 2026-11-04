// safeNext returns the path to go to after logging in: the `next` the login
// page was given when it is a path of this app, "/" otherwise. A protocol-
// relative URL ("//elsewhere.example") would leave the app, and browsers read
// a backslash as a slash and drop tabs and newlines, so none of these is
// accepted.
export function safeNext(next: string | null | undefined): string {
  if (!next || !next.startsWith("/") || next.startsWith("//")) return "/";
  // eslint-disable-next-line no-control-regex
  if (/[\\\u0000-\u001f\u007f]/.test(next)) return "/";
  if (next === "/login" || next.startsWith("/login?") || next.startsWith("/login#")) return "/";
  return next;
}
