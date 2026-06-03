import { auth, signOut } from "@/auth";
import { redirect } from "next/navigation";
import { ensureCustomer, listKeys, sumUsage, listAuditEvents, AuditEventRow } from "@/lib/db";
import { CreateKeyForm, RevokeKeyButton } from "./create-key-form";
import { SignOutButton } from "./sign-out-button";

export const dynamic = "force-dynamic";

function getAuditEventLabel(e: AuditEventRow): string {
  const details =
    typeof e.details === "object" && e.details !== null
      ? (e.details as Record<string, unknown>)
      : null;
  if (typeof details?.prefix === "string") return details.prefix;
  if (typeof details?.name === "string") return details.name;
  if (e.target_type && e.target_id) return `${e.target_type}:${e.target_id.slice(0, 8)}`;
  if (e.target_id) return e.target_id.slice(0, 8);
  // No supplementary label — action is already rendered in its own span.
  return "";
}

export default async function DashboardPage() {
  const session = await auth();
  if (!session?.user?.email) {
    redirect("/login");
  }
  const customer = await ensureCustomer(session.user.email);
  const [keys, usage, auditEvents] = await Promise.all([
    listKeys(customer.id),
    sumUsage(customer.id, 30),
    listAuditEvents(customer.id),
  ]);

  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-3xl">
        <header className="flex justify-between items-center mb-6 sm:mb-8">
          <h1 className="text-2xl sm:text-3xl font-bold">Dashboard</h1>
          <form
            action={async () => {
              "use server";
              await signOut();
            }}
          >
            <SignOutButton />
          </form>
        </header>

        <section className="border border-zinc-200 rounded-lg p-4 sm:p-5 mb-5 sm:mb-6">
          <div className="text-sm text-zinc-500">Signed in as</div>
          <div className="text-base sm:text-lg break-all">{session.user.email}</div>
          <div className="mt-3 text-sm text-zinc-500">Plan</div>
          <div className="text-base sm:text-lg font-medium uppercase">{customer.plan_id}</div>
        </section>

        <section className="border border-zinc-200 rounded-lg p-4 sm:p-5 mb-5 sm:mb-6" aria-label="API keys">
          <div className="flex flex-col sm:flex-row sm:justify-between sm:items-center gap-3 mb-3">
            <h2 className="text-lg sm:text-xl font-semibold">API keys</h2>
            <CreateKeyForm existingNames={keys.map((k) => k.name ?? "")} />
          </div>
          {keys.length === 0 ? (
            <p className="text-sm text-zinc-500">No keys yet.</p>
          ) : (
            <ul className="space-y-2">
              {keys.map((k) => (
                <li key={k.id} className="flex items-center justify-between gap-3 text-sm">
                  <span className="font-mono break-all">
                    {k.prefix}…{" "}
                    <span className="text-zinc-500">
                      ({k.name || "unnamed"} · last used{" "}
                      {k.last_used_at ? new Date(k.last_used_at).toLocaleString() : "never"})
                    </span>
                  </span>
                  <RevokeKeyButton keyId={k.id} keyPrefix={k.prefix} />
                </li>
              ))}
            </ul>
          )}
        </section>

        <section className="border border-zinc-200 rounded-lg p-4 sm:p-5 mb-5 sm:mb-6" aria-label="Usage stats">
          <h2 className="text-lg sm:text-xl font-semibold mb-3">Usage (last 30 days)</h2>
          <div className="text-3xl sm:text-4xl font-bold font-variant-numeric-tabular">{usage.toLocaleString()}</div>
          <div className="text-sm text-zinc-500">billable units</div>
        </section>

        <section className="border border-zinc-200 rounded-lg p-4 sm:p-5" aria-label="Recent activity">
          <h2 className="text-lg sm:text-xl font-semibold mb-3">Recent activity</h2>
          {auditEvents.length === 0 ? (
            <p className="text-sm text-zinc-500">No recent activity.</p>
          ) : (
            <ul className="space-y-2">
              {auditEvents.map((e) => {
                const label = getAuditEventLabel(e);
                return (
                  <li key={e.id} className="flex items-center justify-between text-sm gap-2">
                    <span className="font-mono text-zinc-800">{e.action}</span>
                    {label && <span className="text-zinc-500 text-xs">{label}</span>}
                    <span className="text-zinc-400 text-xs ml-auto whitespace-nowrap">
                      {new Date(e.created_at).toLocaleString()}
                    </span>
                  </li>
                );
              })}
            </ul>
          )}
        </section>
      </div>
    </main>
  );
}
