import { ScanForm } from "@/components/scan-form";

export default function Home() {
  return (
    <div className="mx-auto max-w-xl pt-12">
      <h1 className="text-3xl font-semibold tracking-tight">Website Mapper</h1>
      <p className="mt-2 text-muted">
        Understand what is publicly discoverable about your website.
      </p>

      <div className="mt-8">
        <ScanForm />
      </div>

      <section className="mt-12 space-y-3 text-sm leading-6 text-muted">
        <h2 className="font-medium text-fg">What the scan does</h2>
        <p>
          Starting from the address you enter, the scanner fetches public pages on your domain
          and its subdomains, follows links a few levels deep, and records every page, API-like
          endpoint and asset it can see. For each result it shows how it was found.
        </p>
        <p>
          Discovery is passive and low-impact: a limited number of ordinary GET requests at a
          modest rate. Nothing is submitted, guessed or brute-forced. Only scan domains you own
          or have permission to inspect.
        </p>
        <p>
          The result is a map of what the scanner could discover publicly — not a complete
          inventory of every route your server implements.
        </p>
      </section>
    </div>
  );
}
