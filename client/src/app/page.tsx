import { ScanForm } from "@/components/scan-form";

export default function Home() {
  return (
    <div className="mx-auto max-w-xl pt-12">
      <h1 className="text-3xl font-semibold tracking-tight">Web Scanner</h1>
      <p className="mt-2 text-muted">
        Understand what is publicly discoverable about your website.
      </p>

      <div className="mt-8">
        <ScanForm />
      </div>

      <section className="mt-12 space-y-3 text-sm leading-6 text-muted">
        <h2 className="font-medium text-fg">What the scan does</h2>
        <p>
          Every scan lists your domain&rsquo;s subdomains from public certificate logs and the
          routes a public web archive has recorded. Standard scans also check which subdomains
          are live and read their robots.txt and sitemaps; a full crawl additionally follows
          links on each live site. For each result the map shows how it was found.
        </p>
        <p>
          Discovery is low-impact: a passive scan never contacts your site, and the others make
          a limited number of ordinary GET requests at a modest rate. Nothing is submitted,
          guessed or brute-forced. Only scan domains you own or have permission to inspect.
        </p>
        <p>
          The result is a map of what the scanner could discover publicly — not a complete
          inventory of every route your server implements.
        </p>
      </section>
    </div>
  );
}
