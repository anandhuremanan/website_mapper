"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { ResultsView } from "@/components/results/results-view";
import { ScanProgress } from "@/components/scan-progress";
import { isFinished } from "@/lib/types";
import { useScan } from "@/lib/use-scan";

export default function ScanPage() {
  const { id } = useParams<{ id: string }>();
  const { scan, result, error, notFound } = useScan(id);

  if (notFound) {
    return (
      <div className="space-y-2">
        <h1 className="text-xl font-semibold">Scan not found</h1>
        <p className="text-sm text-muted">
          Scans are kept in memory: they disappear when the server restarts, and the oldest
          finished scans are cleared to free memory.{" "}
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
      {finished && result ? (
        <ResultsView scan={scan} result={result} />
      ) : (
        <ScanProgress scan={scan} />
      )}
    </div>
  );
}
