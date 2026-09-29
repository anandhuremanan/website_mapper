import { FoundThrough } from "@/components/source-list";
import { assetKindLabels, statusTone, typeLabels } from "@/lib/format";
import type { DiscoveredUrl } from "@/lib/types";

export function UrlDetail({ url }: { url: DiscoveredUrl }) {
  const typeText = url.assetKind
    ? `${typeLabels[url.type]} · ${assetKindLabels[url.assetKind]}`
    : typeLabels[url.type];

  return (
    <dl className="grid grid-cols-[8rem_1fr] gap-x-4 gap-y-2 text-sm">
      <Row label="URL">
        <a
          href={url.url}
          target="_blank"
          rel="noopener noreferrer nofollow"
          className="break-all font-mono text-accent underline-offset-2 hover:underline"
        >
          {url.url}
        </a>
      </Row>
      <Row label="Status">
        {url.status ? (
          <span className={`font-mono ${statusTone(url.status)}`}>{url.status}</span>
        ) : (
          <span className="text-muted">
            {url.error ? "Request failed" : "Not requested by the scanner"}
          </span>
        )}
      </Row>
      {url.error && (
        <Row label="Error">
          <span className="break-all text-warn">{url.error}</span>
        </Row>
      )}
      {url.contentType && (
        <Row label="Content type">
          <span className="font-mono">{url.contentType}</span>
        </Row>
      )}
      <Row label="Classified as">
        {typeText}
        <span className="block text-muted">{url.typeEvidence}</span>
      </Row>
      {url.methods && url.methods.length > 0 && (
        <Row label="Methods">
          <span className="font-mono">{url.methods.join(", ")}</span>
          {url.methods.includes("POST") && (
            <span className="block text-muted">Seen as a form target; never submitted.</span>
          )}
        </Row>
      )}
      {url.redirect && (
        <Row label="Redirects to">
          <span className="break-all font-mono">{url.redirect}</span>
        </Row>
      )}
      {url.title && <Row label="Title">{url.title}</Row>}
      {url.server && (
        <Row label="Server">
          <span className="font-mono">{url.server}</span>
        </Row>
      )}
      <Row label="Found through">
        <FoundThrough sources={url.sources} />
      </Row>
      {url.discoveredFrom && url.discoveredFrom.length > 0 && (
        <Row label="Referenced on">
          <ul className="space-y-0.5">
            {url.discoveredFrom.map((from) => (
              <li key={from} className="break-all font-mono text-muted">
                {from}
              </li>
            ))}
          </ul>
        </Row>
      )}
    </dl>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  );
}
