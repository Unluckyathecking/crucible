import { OperatorNav } from "../_components/operator-nav";
import { listPlans } from "@/lib/operator/client";

export const dynamic = "force-dynamic";

export default async function OperatorPlansPage() {
  const plans = await listPlans();

  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-5xl">
        <OperatorNav />
        <h1 className="text-2xl font-bold mb-4">Plans</h1>

        <div className="overflow-x-auto border border-zinc-200 dark:border-zinc-700 rounded-lg">
          <table className="w-full text-sm">
            <thead className="bg-zinc-50 dark:bg-zinc-800 text-left">
              <tr>
                <th className="px-3 py-2">ID</th>
                <th className="px-3 py-2">Name</th>
                <th className="px-3 py-2">Rate limit / min</th>
                <th className="px-3 py-2">Monthly unit cap</th>
                <th className="px-3 py-2">Stripe price</th>
              </tr>
            </thead>
            <tbody>
              {plans.map((plan) => (
                <tr key={plan.id} className="border-t border-zinc-200 dark:border-zinc-700">
                  <td className="px-3 py-2">{plan.id}</td>
                  <td className="px-3 py-2">{plan.display_name}</td>
                  <td className="px-3 py-2">{plan.rate_limit_per_minute}</td>
                  <td className="px-3 py-2">{plan.monthly_unit_cap ?? "—"}</td>
                  <td className="px-3 py-2">{plan.stripe_price_id ?? "—"}</td>
                </tr>
              ))}
              {plans.length === 0 && (
                <tr>
                  <td className="px-3 py-4 text-zinc-500" colSpan={5}>
                    No plans configured.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>
    </main>
  );
}
