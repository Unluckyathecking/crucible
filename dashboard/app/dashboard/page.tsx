import { auth, signOut } from "@/auth";
import { redirect } from "next/navigation";
import { ensureCustomer, listKeys, sumUsage } from "@/lib/db";

export const dynamic = "force-dynamic";

export default async function DashboardPage() {
  const session = await auth();
  if (!session?.user?.email) {
    redirect("/login");
  }
  const customer = await ensureCustomer(session.user.email);
  const keys = await listKeys(customer.id);
  const usage = await sumUsage(customer.id, 30);

  return (
    <main className="min-h-screen p-8 max-w-3xl mx-auto">
      <header className="flex justify-between items-center mb-8">
        <h1 className="text-3xl font-bold">Dashboard</h1>
        <form
          action={async () => {
            "use server";
            await signOut();
          }}
        >
          <button className="text-sm text-zinc-500 hover:underline">Sign out</button>
        </form>
      </header>

      <section className="border border-zinc-200 rounded-lg p-5 mb-6">
        <div className="text-sm text-zinc-500">Signed in as</div>
        <div className="text-lg">{session.user.email}</div>
        <div className="mt-3 text-sm text-zinc-500">Plan</div>
        <div className="text-lg font-medium uppercase">{customer.plan_id}</div>
      </section>

      <section className="border border-zinc-200 rounded-lg p-5 mb-6">
        <div className="flex justify-between items-center mb-3">
          <h2 className="text-xl font-semibold">API keys</h2>
          <form action="/api/keys" method="POST">
            <button className="px-3 py-1 bg-zinc-900 text-white rounded text-sm hover:bg-zinc-700 transition">
              Create key
            </button>
          </form>
        </div>
        {keys.length === 0 ? (
          <p className="text-sm text-zinc-500">No keys yet.</p>
        ) : (
          <ul className="space-y-2">
            {keys.map((k) => (
              <li key={k.id} className="text-sm font-mono">
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

      <section className="border border-zinc-200 rounded-lg p-5">
        <h2 className="text-xl font-semibold mb-3">Usage (last 30 days)</h2>
        <div className="text-4xl font-bold">{usage.toLocaleString()}</div>
        <div className="text-sm text-zinc-500">billable units</div>
      </section>
    </main>
  );
}
