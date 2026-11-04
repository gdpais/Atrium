import type { components } from "./schema.gen";

export type Problem = components["schemas"]["Problem"];

// An answer from the API that is not a success, with its problem details
// (RFC 9457) when the body carried them.
export class ApiError extends Error {
  readonly status: number;
  readonly problem: Problem | undefined;
  readonly response: Response;

  constructor(response: Response, problem?: unknown) {
    const details = isProblem(problem) ? problem : undefined;
    super(details?.detail ?? details?.title ?? `the API answered ${response.status}`);
    this.name = "ApiError";
    this.status = response.status;
    this.problem = details;
    this.response = response;
  }
}

function isProblem(value: unknown): value is Problem {
  return typeof value === "object" && value !== null && typeof (value as Problem).title === "string";
}

// unwrap returns an openapi-fetch result's data, or throws an ApiError when
// the API did not answer with a success.
export function unwrap<T>(result: { data?: T; error?: unknown; response: Response }): T {
  if (!result.response.ok) throw new ApiError(result.response, result.error);
  return result.data as T;
}

export function isStatus(error: unknown, status: number): error is ApiError {
  return error instanceof ApiError && error.status === status;
}
