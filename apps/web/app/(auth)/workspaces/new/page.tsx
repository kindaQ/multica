"use client";

import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { useAuthStore } from "@multica/core/auth";
import { paths } from "@multica/core/paths";
import { workspaceListOptions } from "@multica/core/workspace/queries";
import { NewWorkspacePage } from "@multica/views/workspace/new-workspace-page";

function NewWorkspacePageContent() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const user = useAuthStore((s) => s.user);
  const isLoading = useAuthStore((s) => s.isLoading);
  const { data: wsList = [] } = useQuery({
    ...workspaceListOptions(),
    enabled: !!user,
  });

  useEffect(() => {
    if (!isLoading && !user) router.replace(paths.login());
  }, [isLoading, user, router]);

  if (isLoading || !user) return null;

  // Back goes to the root path — the workspace layout redirects from
  // there to the user's default workspace. Only show Back when there's
  // somewhere to go back to (user already has at least one workspace).
  const onBack =
    wsList.length > 0 ? () => router.push(paths.root()) : undefined;

  return (
    <NewWorkspacePage
      onSuccess={(ws) => {
        const next = searchParams.get("next");
        router.push(next?.startsWith("/") && !next.startsWith("//") ? next : paths.workspace(ws.slug).issues());
      }}
      onBack={onBack}
    />
  );
}

export default function Page() {
  return (
    <Suspense fallback={null}>
      <NewWorkspacePageContent />
    </Suspense>
  );
}
