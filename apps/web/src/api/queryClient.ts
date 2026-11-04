import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";
import { ApiError, isStatus } from "./errors";
import { signedOut } from "./session";

// createQueryClient makes the app's one QueryClient. A 401 from any request
// means the session ended (expired, logged out elsewhere, the user disabled),
// so the app forgets what it loaded and shows the login page, which brings
// the user back where they were. A 4xx is the API's answer, not a failure
// worth repeating, so only other errors are retried.
export function createQueryClient(): QueryClient {
  const onError = (error: unknown) => {
    if (isStatus(error, 401)) signedOut(client);
  };
  const client: QueryClient = new QueryClient({
    queryCache: new QueryCache({ onError }),
    mutationCache: new MutationCache({ onError }),
    defaultOptions: {
      queries: {
        retry: (failures, error) => !(error instanceof ApiError && error.status < 500) && failures < 1,
        refetchOnWindowFocus: false,
      },
    },
  });
  return client;
}
