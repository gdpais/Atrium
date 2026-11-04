import { MantineProvider } from "@mantine/core";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";
import { createBrowserRouter, RouterProvider, type RouteObject } from "react-router";
import { Home } from "./pages/Home";
import { NotFound } from "./pages/NotFound";
import { theme } from "./theme";

export const routes: RouteObject[] = [
  { path: "/", element: <Home /> },
  { path: "*", element: <NotFound /> },
];

export function Providers({ children }: { children: React.ReactNode }) {
  const [client] = useState(
    () => new QueryClient({ defaultOptions: { queries: { retry: 1, refetchOnWindowFocus: false } } }),
  );
  return (
    <MantineProvider theme={theme}>
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    </MantineProvider>
  );
}

export function App() {
  const [router] = useState(() => createBrowserRouter(routes));
  return (
    <Providers>
      <RouterProvider router={router} />
    </Providers>
  );
}
