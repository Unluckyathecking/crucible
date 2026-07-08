import Link from "next/link";

const GITHUB_URL = "https://github.com/Unluckyathecking/crucible";
const LICENSING_DOC_URL = `${GITHUB_URL}/blob/main/docs/pricing.md`;
const SALES_MAILTO = "mailto:sales@crucible.dev?subject=Crucible%20licence";
const CLOUD_WAITLIST_MAILTO = "mailto:sales@crucible.dev?subject=Crucible%20Cloud%20waitlist";

const features: { title: string; body: string }[] = [
  {
    title: "API key auth",
    body: "Salted SHA-256 keys with a Redis hot cache and constant-time verification. Issue from the dashboard, verify at the gateway.",
  },
  {
    title: "Rate limiting",
    body: "Per-customer sliding-window limits enforced with atomic Lua scripts, tiered by plan.",
  },
  {
    title: "Monthly quota",
    body: "Per-plan monthly unit caps with atomic reserve — no overspend, no double-count under load.",
  },
  {
    title: "Stripe metered billing",
    body: "Async batch flusher and HMAC-verified webhooks meter every billable unit to your Stripe account.",
  },
  {
    title: "Observability",
    body: "Prometheus metrics (counters and histograms, cardinality capped) and health-check endpoints out of the box.",
  },
  {
    title: "Customer dashboard",
    body: "Next.js 15 dashboard with NextAuth magic-link sign-in, key management, and usage analytics.",
  },
  {
    title: "Worker SDKs",
    body: "Write product logic once against a frozen HTTP/JSON contract. Go and Rust SDKs ship today; TypeScript follows.",
  },
  {
    title: "OpenAPI & docs",
    body: "OpenAPI 3.1 document and SDK generation from the same contract your worker already speaks.",
  },
];

type Tier = {
  name: string;
  price: string;
  cadence?: string;
  blurb: string;
  bullets: string[];
  cta: { label: string; href: string };
  featured?: boolean;
};

const tiers: Tier[] = [
  {
    name: "Community",
    price: "Free",
    blurb: "Self-host the full MIT core.",
    bullets: [
      "Gateway: auth, rate limiting, quota",
      "Stripe metered billing for your customers",
      "Worker SDKs (Go, Rust, TypeScript)",
      "Dashboard, observability stack, docs",
    ],
    cta: { label: "View on GitHub", href: GITHUB_URL },
  },
  {
    name: "Pro",
    price: "£39",
    cadence: "/month, or £390/year",
    blurb: "License key unlocks Enterprise Edition features.",
    bullets: [
      "Operator multi-token access",
      "Customer audit log export",
      "Priority email support",
      "All future Pro features",
    ],
    cta: { label: "Contact sales", href: SALES_MAILTO },
    featured: true,
  },
  {
    name: "Business",
    price: "£249",
    cadence: "/month",
    blurb: "Everything in Pro, plus:",
    bullets: [
      "Dashboard SSO (OIDC)",
      "Deployment support",
      "SLA-backed support",
      "Teams / RBAC, multi-project (coming soon)",
    ],
    cta: { label: "Contact sales", href: SALES_MAILTO },
  },
  {
    name: "Enterprise",
    price: "Custom",
    blurb: "Everything in Business, plus:",
    bullets: [
      "White-label / embedding rights",
      "SAML (coming soon)",
      "Compliance documentation",
      "Custom terms",
    ],
    cta: { label: "Contact sales", href: SALES_MAILTO },
  },
];

export default function Home() {
  return (
    <main id="main-content" className="min-h-screen">
      <header className="mx-auto w-full max-w-5xl px-6 pt-6 flex items-center justify-between">
        <span className="text-lg font-bold tracking-tight">Crucible</span>
        <nav className="flex items-center gap-4 text-sm">
          <a href={GITHUB_URL} className="text-muted-foreground hover:text-foreground">
            GitHub
          </a>
          <Link
            href="/login"
            className="px-4 py-1.5 border border-border rounded hover:bg-muted transition"
          >
            Sign in
          </Link>
        </nav>
      </header>

      <section className="mx-auto w-full max-w-5xl px-6 py-16 sm:py-24 text-center flex flex-col items-center gap-6">
        <h1 className="text-4xl sm:text-6xl font-bold tracking-tight max-w-3xl">
          Idea to a metered, billed, production API in a day.
        </h1>
        <p className="text-lg text-muted-foreground max-w-2xl">
          Crucible is a clone-and-adapt framework for high-volume metered API products. One Go
          gateway handles auth, rate limiting, quota, Stripe metered billing, and observability —
          so you ship product logic in a single worker and never touch the billing, auth, or quota
          plumbing.
        </p>
        <div className="flex flex-wrap gap-3 justify-center">
          <a
            href={GITHUB_URL}
            className="px-5 py-2.5 bg-primary text-primary-foreground rounded hover:opacity-90 transition font-medium"
          >
            Get the source
          </a>
          <a
            href="#pricing"
            className="px-5 py-2.5 border border-border rounded hover:bg-muted transition font-medium"
          >
            See pricing
          </a>
        </div>
      </section>

      <section className="mx-auto w-full max-w-5xl px-6 pb-16" aria-labelledby="features-heading">
        <h2 id="features-heading" className="text-2xl font-bold mb-2">
          What the core gives you
        </h2>
        <p className="text-muted-foreground mb-8">
          The MIT core is identical across every clone. Adapt one worker; everything below is
          already built and tested.
        </p>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {features.map((f) => (
            <div key={f.title} className="border border-border rounded-lg p-5">
              <h3 className="font-semibold mb-1.5">{f.title}</h3>
              <p className="text-sm text-muted-foreground">{f.body}</p>
            </div>
          ))}
        </div>
      </section>

      <section
        id="pricing"
        className="mx-auto w-full max-w-5xl px-6 py-12 scroll-mt-6"
        aria-labelledby="pricing-heading"
      >
        <h2 id="pricing-heading" className="text-2xl font-bold mb-2">
          Pricing
        </h2>
        <p className="text-muted-foreground mb-8">
          Open core: the MIT core is free forever. Paid tiers unlock source-available Enterprise
          Edition features via an offline licence key. Prices exclude VAT.
        </p>
        <div className="grid gap-4 lg:grid-cols-4">
          {tiers.map((t) => (
            <div
              key={t.name}
              className={`flex flex-col border rounded-lg p-5 ${
                t.featured ? "border-foreground" : "border-border"
              }`}
            >
              <h3 className="text-lg font-semibold">{t.name}</h3>
              <div className="mt-2 mb-1">
                <span className="text-3xl font-bold">{t.price}</span>
                {t.cadence && (
                  <span className="text-sm text-muted-foreground"> {t.cadence}</span>
                )}
              </div>
              <p className="text-sm text-muted-foreground mb-4">{t.blurb}</p>
              <ul className="space-y-2 text-sm mb-6 flex-1">
                {t.bullets.map((b) => (
                  <li key={b} className="flex gap-2">
                    <span aria-hidden="true" className="text-muted-foreground">
                      •
                    </span>
                    <span>{b}</span>
                  </li>
                ))}
              </ul>
              <a
                href={t.cta.href}
                className={`text-center px-4 py-2 rounded font-medium transition ${
                  t.featured
                    ? "bg-primary text-primary-foreground hover:opacity-90"
                    : "border border-border hover:bg-muted"
                }`}
              >
                {t.cta.label}
              </a>
            </div>
          ))}
        </div>

        <div className="mt-4 border border-dashed border-border rounded-lg p-5 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-3">
          <div>
            <h3 className="text-lg font-semibold">Crucible Cloud</h3>
            <p className="text-sm text-muted-foreground">
              A hosted control plane — fully managed gateway, dashboard, and metering. In
              development.
            </p>
          </div>
          <a
            href={CLOUD_WAITLIST_MAILTO}
            className="shrink-0 text-center px-4 py-2 border border-border rounded font-medium hover:bg-muted transition"
          >
            Join the waitlist
          </a>
        </div>
      </section>

      <section className="mx-auto w-full max-w-5xl px-6 py-12" aria-labelledby="licensing-heading">
        <div className="border border-border rounded-lg p-6">
          <h2 id="licensing-heading" className="text-xl font-bold mb-2">
            Editions &amp; licensing
          </h2>
          <p className="text-sm text-muted-foreground max-w-3xl">
            The Crucible core is open source under the MIT license — clone it, self-host it, ship
            your product on it with no strings. Enterprise Edition features are source-available and
            unlocked by an offline Ed25519 license key (<code>CRUCIBLE_LICENSE_KEY</code>); no
            phone-home, no runtime dependency on us. If a license lapses, EE features keep running
            for a 14-day grace period, then disable — the MIT core is unaffected.
          </p>
          <p className="text-sm mt-4">
            <a href={LICENSING_DOC_URL} className="underline hover:no-underline">
              Read the full pricing &amp; licensing details →
            </a>
          </p>
        </div>
      </section>

      <footer className="mx-auto w-full max-w-5xl px-6 py-10 border-t border-border text-sm text-muted-foreground flex flex-wrap gap-x-6 gap-y-2 justify-between">
        <span>Crucible — MIT core, source-available EE.</span>
        <span className="flex gap-4">
          <a href={GITHUB_URL} className="hover:text-foreground">
            GitHub
          </a>
          <Link href="/login" className="hover:text-foreground">
            Sign in
          </Link>
        </span>
      </footer>
    </main>
  );
}
