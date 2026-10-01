"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { liveSummary, ResultsView } from "@/components/results/results-view";
import { ScanProgress } from "@/components/scan-progress";
import { isFinished } from "@/lib/types";
import { useScan } from "@/lib/use-scan";

export default function ScanPage() {
  const { id } = useParams<{ id: string }>();
  const { scan, summary, error, notFound } = useScan(id);

  if (notFound) {
    return (
      <div className="space-y-2">
        <h1 className="text-xl font-semibold">Scan not found</h1>
        <p className="text-sm text-muted">
          Finished scans are kept for a limited time and then removed to free space, oldest
          first.{" "}
          <Link href="/" className="text-accent underline-offset-2 hover:underline">
            Start a new scan
          </Link>
        </p>
      </div>
    );
  }

  if (!scan) {
    return <p className="text-sm text-muted">{error ?? "Loading…"}</p>;
  }

  const finished = isFinished(scan.status);

  return (
    <div className="space-y-8">
      {error && (
        <p role="alert" className="text-sm text-warn">
          {error}
        </p>
      )}
      {!(finished && summary) && <ScanProgress scan={scan} />}
      {/* What a running scan has found can be browsed before it ends. The
          same view then shows the final result, keeping the open tab. */}
      {finished && summary ? (
        <ResultsView scan={scan} result={summary} />
      ) : scan.status === "running" && scan.counts.hosts > 0 ? (
        <ResultsView scan={scan} result={liveSummary(scan)} live />
      ) : null}
    </div>
  );
}
