import type { Metadata } from "next";
import type { BotInfo } from "@/lib/types";

export const metadata: Metadata = {
  title: "WebsiteMapperBot — Website Mapper",
  description: "What the Website Mapper crawler does and how to contact its operator.",
};

const apiUrl = process.env.API_URL ?? "http://localhost:8080";

async function loadBotInfo(): Promise<BotInfo | null> {
  try {
    const res = await fetch(`${apiUrl}/api/bot`, { cache: "no-store" });
    return res.ok ? ((await res.json()) as BotInfo) : null;
  } catch {
    return null;
  }
}

/**
 * Public page for site owners who see the crawler in their logs. The
 * User-Agent links here.
 */
export default async function BotPage() {
  const bot = await loadBotInfo();
  const token = bot?.robotsToken ?? "WebsiteMapperBot";

  return (
    <article className="mx-auto max-w-2xl space-y-8 text-sm leading-6">
      <header className="space-y-2">
        <h1 className="text-2xl font-semibold tracking-tight">{token}</h1>
        <p className="text-muted">
          The crawler behind Website Mapper, a tool that shows people what is publicly
          discoverable about websites they own or are permitted to assess.
        </p>
      </header>

      <section className="space-y-2">
        <h2 className="font-medium">How to recognize it</h2>
        <p>Every request it makes sends this User-Agent:</p>
        <pre className="overflow-x-auto rounded-md border border-border bg-surface px-3 py-2 font-mono text-xs">
          {bot?.userAgent ?? `${token}/0.1 (+<this page>)`}
        </pre>
      </section>

      <section className="space-y-2">
        <h2 className="font-medium">What it does</h2>
        <ul className="list-disc space-y-1 pl-5">
          <li>
            Looks up hostnames for a domain in public Certificate Transparency logs and resolves
            them in DNS.
          </li>
          <li>
            Requests the home page of each host over HTTPS (or HTTP) once, to see whether it
            answers.
          </li>
          <li>
            Crawls reachable hosts through ordinary <code className="font-mono">GET</code>{" "}
            requests, following links a few levels deep, and records metadata: URL, status code,
            content type and page title.
          </li>
        </ul>
        <p>This is passive HTTP/HTTPS discovery. The crawler does not:</p>
        <ul className="list-disc space-y-1 pl-5">
          <li>submit forms, send request bodies, or use any method other than GET;</li>
          <li>log in, send cookies or credentials, or guess passwords;</li>
          <li>guess paths or subdomains from wordlists, or scan ports;</li>
          <li>test for or exploit vulnerabilities;</li>
          <li>store page contents (only the metadata listed above is kept).</li>
        </ul>
        <p>
          It only runs when a user starts a scan and confirms they own the domain or have
          permission to scan it.
        </p>
      </section>

      <section className="space-y-2">
        <h2 className="font-medium">robots.txt</h2>
        {bot && !bot.respectsRobotsTxt ? (
          <p>
            The operator of this instance has turned off robots.txt checking. Contact them below
            if you want your site excluded.
          </p>
        ) : (
          <p>
            Before crawling a host, the crawler reads its{" "}
            <code className="font-mono">/robots.txt</code> and does not request disallowed paths.
            It follows rules for <code className="font-mono">User-agent: {token}</code>, or for{" "}
            <code className="font-mono">User-agent: *</code> if there is no specific group. To
            exclude your site entirely:
          </p>
        )}
        <pre className="overflow-x-auto rounded-md border border-border bg-surface px-3 py-2 font-mono text-xs">
          {`User-agent: ${token}\nDisallow: /`}
        </pre>
        <p className="text-muted">
          If robots.txt cannot be fetched, the crawl continues normally. The single request that
          checks whether a host answers is made before robots.txt is read.
        </p>
      </section>

      {bot && (
        <section className="space-y-2">
          <h2 className="font-medium">Limits</h2>
          <ul className="list-disc space-y-1 pl-5">
            <li>At most {bot.limits.requestsPerSecondPerHost} requests per second to any one host.</li>
            <li>
              At most {bot.limits.maxRequestsPerHost} page requests per host, and{" "}
              {bot.limits.maxRequestsPerScan.toLocaleString()} requests per scan in total (no more
              than {bot.limits.requestsPerSecondPerScan} per second).
            </li>
            <li>At most {bot.limits.maxHostsCrawled} hosts crawled per scan.</li>
            <li>Requests time out after {bot.limits.requestTimeoutSeconds} seconds.</li>
            <li>Private, loopback and reserved network addresses are never contacted.</li>
          </ul>
        </section>
      )}

      <section className="space-y-2">
        <h2 className="font-medium">Contact</h2>
        {bot?.contact ? (
          <p>
            Questions, or want your site excluded? Contact the operator of this instance:{" "}
            {bot.contact.includes("@") && !bot.contact.includes("://") ? (
              <a href={`mailto:${bot.contact}`} className="text-accent hover:underline">
                {bot.contact}
              </a>
            ) : (
              <a href={bot.contact} className="text-accent hover:underline" rel="noopener noreferrer">
                {bot.contact}
              </a>
            )}
          </p>
        ) : (
          <p className="text-muted">
            The operator of this instance has not published contact details. You can block the
            crawler with the robots.txt rule above.
          </p>
        )}
      </section>

      {!bot && (
        <p className="text-xs text-muted">
          Live settings could not be loaded from the scanner, so general information is shown.
        </p>
      )}
    </article>
  );
}
