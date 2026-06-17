import { Skeleton } from "./ui/Skeleton";

/**
 * RouteFallback is the Suspense placeholder shown while a lazily
 * loaded route chunk is fetched. Routes are code-split in App.tsx, so
 * this renders inside the Layout shell (the nav stays mounted) and
 * only needs to fill the content area with a lightweight skeleton.
 */
export function RouteFallback(): JSX.Element {
  return (
    <div role="status" aria-busy="true" className="flex flex-col gap-4">
      <span className="visually-hidden">Loading…</span>
      <Skeleton width="14rem" height="1.75rem" />
      <Skeleton lines={6} />
    </div>
  );
}
