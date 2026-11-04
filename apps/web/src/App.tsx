import { MantineProvider } from "@mantine/core";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { createBrowserRouter, RouterProvider, type RouteObject } from "react-router";
import { createQueryClient } from "./api/queryClient";
import { RequireRole, RequireSession } from "./components/Guards";
import { Layout } from "./components/Layout";
import { sections } from "./nav";
import { Applications } from "./pages/Applications";
import { Environments } from "./pages/Environments";
import { Home } from "./pages/Home";
import { Login } from "./pages/Login";
import { NotFound } from "./pages/NotFound";
import { RouteError } from "./pages/RouteError";
import { Section } from "./pages/Section";
import { theme } from "./theme";

// The screens built so far; the other sections show a placeholder.
const screens: Record<string, React.ReactNode> = {
  "/environments": <Environments />,
  "/applications": <Applications />,
};

// Every page but the login page needs a session; a section's page needs the
// role its navigation entry names, so a link typed by hand is gated too.
export const routes: RouteObject[] = [
  { path: "/login", element: <Login />, errorElement: <RouteError /> },
  {
    element: <RequireSession />,
    errorElement: <RouteError />,
    children: [
      {
        element: <Layout />,
        children: [
          { index: true, element: <Home /> },
          ...sections.map((section) => ({
            element: <RequireRole role={section.role} />,
            children: [{ path: section.path, element: screens[section.path] ?? <Section title={section.label} /> }],
          })),
          { path: "*", element: <NotFound /> },
        ],
      },
    ],
  },
];

export function Providers({ children, client }: { children: React.ReactNode; client?: QueryClient }) {
  const [queryClient] = useState(() => client ?? createQueryClient());
  return (
    <MantineProvider theme={theme}>
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
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
