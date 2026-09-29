import type { Scan, StepStatus } from "@/lib/types";

const stepIcon: Record<StepStatus, { icon: string; className: string }> = {
  pending: { icon: "○", className: "text-muted" },
  running: { icon: "●", className: "text-accent animate-pulse" },
  done: { icon: "✓", className: "text-ok" },
  failed: { icon: "✕", className: "text-warn" },
  skipped: { icon: "–", className: "text-muted" },
};

export function ScanProgress({ scan }: { scan: Scan }) {
  const { counts } = scan;
  const heading =
    scan.status === "queued"
      ? "Waiting to start…"
      : scan.status === "failed"
        ? "Scan failed"
        : "Scanning…";

  return (
    <div className="max-w-xl space-y-8">
      <div>
        <h1 className="font-mono text-2xl font-semibold tracking-tight">{scan.domain}</h1>
        <p className="mt-1 text-sm text-muted">{heading}</p>
        {scan.error && <p className="mt-2 text-sm text-danger">{scan.error}</p>}
      </div>

      <dl className="grid grid-cols-2 gap-x-8 gap-y-3 text-sm sm:grid-cols-3">
        <Count label="Hosts discovered" value={counts.hosts} />
        <Count label="Hosts resolved" value={counts.hostsResolved} />
        <Count label="Hosts reachable" value={counts.hostsReachable} />
        <Count label="URLs discovered" value={counts.urls} />
        <Count label="API-like endpoints" value={counts.apis} />
        <Count label="JavaScript files" value={counts.javascript} />
      </dl>

      <ol className="space-y-2 text-sm">
        {scan.steps.map((step) => {
          const { icon, className } = stepIcon[step.status];
          return (
            <li key={step.id} className="flex items-center gap-3">
              <span aria-hidden className={`w-4 text-center ${className}`}>
                {icon}
              </span>
              <span className={step.status === "pending" ? "text-muted" : ""}>{step.label}</span>
              <span className="sr-only">({step.status})</span>
            </li>
          );
        })}
      </ol>
    </div>
  );
}

function Count({ label, value }: { label: string; value: number }) {
  return (
    <div>
      <dt className="text-muted">{label}</dt>
      <dd className="font-mono text-lg tabular-nums">{value.toLocaleString()}</dd>
    </div>
  );
}
