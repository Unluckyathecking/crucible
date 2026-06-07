import { auth, signOut } from "@/auth";
import { redirect } from "next/navigation";
import Link from "next/link";
import { ensureCustomer, listKeys, usageByOperation, listAuditEvents, AuditEventRow, MS_PER_DAY } from "@/lib/db";
import { CreateKeyForm, RevokeKeyButton } from "./create-key-form";
import { SignOutButton } from "./sign-out-button";

export const dynamic = "force-dynamic";

const USAGE_WINDOW_DAYS = 30;

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
  const now = new Date();
  const tomorrowMidnight = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1));
  // Subtract in milliseconds from tomorrowMidnight for an unambiguous USAGE_WINDOW_DAYS × 24 h window.
  const thirtyDaysAgo = new Date(tomorrowMidnight.getTime() - USAGE_WINDOW_DAYS * MS_PER_DAY);
  const [keys, opBreakdown, auditEvents] = await Promise.all([
    listKeys(customer.id),
    usageByOperation(customer.id, thirtyDaysAgo, tomorrowMidnight),
    listAuditEvents(customer.id),
  ]);
  const cap = BigInt(Number.MAX_SAFE_INTEGER);
  // Math.trunc(Number(x) || 0) guards against non-integer or nullish values from schema drift.
  const rawUnits = opBreakdown.reduce((acc, r) => acc + BigInt(Math.trunc(Number(r.total_billable_units) || 0)), 0n);
  const rawCalls = opBreakdown.reduce((acc, r) => acc + BigInt(Math.trunc(Number(r.event_count) || 0)), 0n);
  const totalUnits = rawUnits > cap ? Number.MAX_SAFE_INTEGER : Number(rawUnits);
  const totalCalls = rawCalls > cap ? Number.MAX_SAFE_INTEGER : Number(rawCalls);

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
          <div className="flex justify-between items-center mb-3">
            <h2 className="text-lg sm:text-xl font-semibold">Usage (last {USAGE_WINDOW_DAYS} days)</h2>
            <Link href="/dashboard/usage" className="text-sm text-zinc-500 hover:text-zinc-900 underline">
              Full analytics →
            </Link>
          </div>
          {opBreakdown.length === 0 ? (
            <p className="text-sm text-zinc-500">No usage in this period.</p>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="text-left text-zinc-500 border-b border-zinc-200">
                    <th className="pb-2 pr-4 font-medium">Operation</th>
                    <th className="pb-2 pr-4 font-medium text-right">Units</th>
                    <th className="pb-2 font-medium text-right">Calls</th>
                  </tr>
                </thead>
                <tbody>
                  {opBreakdown.map((row) => (
                    <tr key={row.operation} className="border-b border-zinc-100">
                      <td className="py-2 pr-4 font-mono">{row.operation}</td>
                      <td className="py-2 pr-4 text-right tabular-nums">{row.total_billable_units.toLocaleString()}</td>
                      <td className="py-2 text-right tabular-nums text-zinc-500">{row.event_count.toLocaleString()}</td>
                    </tr>
                  ))}
                </tbody>
                <tfoot>
                  <tr className="text-zinc-600 font-medium">
                    <td className="pt-2 pr-4">Total</td>
                    <td className="pt-2 pr-4 text-right tabular-nums">{totalUnits.toLocaleString()}</td>
                    <td className="pt-2 text-right tabular-nums text-zinc-500">
                      {totalCalls.toLocaleString()}
                    </td>
                  </tr>
                </tfoot>
              </table>
            </div>
          )}
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
                  <li key={e.id} className="flex items-center justify-between text-sm gap-2 min-w-0">
                    <span className="font-mono text-zinc-800 truncate">{e.action}</span>
                    {label && <span className="text-zinc-500 text-xs truncate">{label}</span>}
                    <span className="text-zinc-400 text-xs ml-auto whitespace-nowrap shrink-0">
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
