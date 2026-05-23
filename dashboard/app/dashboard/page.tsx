import { auth, signOut } from "@/auth";
import { redirect } from "next/navigation";
import { ensureCustomer, listKeys, sumUsage } from "@/lib/db";
import { CreateKeyForm } from "./create-key-form";

export const dynamic = "force-dynamic";

export default async function DashboardPage() {
  const session = await auth();
  if (!session?.user?.email) {
    redirect("/login");
  }
  const customer = await ensureCustomer(session.user.email);
  // ⚡ Bolt: Parallelize independent DB queries to reduce TTFB
  const [keys, usage] = await Promise.all([
    listKeys(customer.id),
    sumUsage(customer.id, 30),
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
            <button className="text-sm text-zinc-500 hover:underline">Sign out</button>
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
                <li key={k.id} className="text-sm font-mono break-all">
                  {k.prefix}…{" "}
                  <span className="text-zinc-500">
                    ({k.name || "unnamed"} · last used{" "}
                    {k.last_used_at ? new Date(k.last_used_at).toLocaleString() : "never"})
                  </span>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section className="border border-zinc-200 rounded-lg p-4 sm:p-5" aria-label="Usage stats">
          <h2 className="text-lg sm:text-xl font-semibold mb-3">Usage (last 30 days)</h2>
          <div className="text-3xl sm:text-4xl font-bold font-variant-numeric-tabular">{usage.toLocaleString()}</div>
          <div className="text-sm text-zinc-500">billable units</div>
        </section>
      </div>
    </main>
  );
}
