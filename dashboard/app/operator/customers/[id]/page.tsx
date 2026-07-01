import Link from "next/link";
import { notFound } from "next/navigation";
import { OperatorNav } from "../../_components/operator-nav";
import { OperatorApiError, getCustomer, getCustomerUsage } from "@/lib/operator/client";

export const dynamic = "force-dynamic";

interface CustomerDetailPageProps {
  params: Promise<{ id: string }>;
  searchParams: Promise<{ start?: string; end?: string }>;
}

export default async function OperatorCustomerDetailPage({ params, searchParams }: CustomerDetailPageProps) {
  const { id } = await params;
  const { start, end } = await searchParams;

  let customer;
  let usage;
  try {
    [customer, usage] = await Promise.all([getCustomer(id), getCustomerUsage(id, { start, end })]);
  } catch (err) {
    if (err instanceof OperatorApiError && (err.status === 404 || err.status === 400)) {
      notFound();
    }
    throw err;
  }

  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-5xl">
        <OperatorNav />
        <Link href="/operator/customers" className="text-sm text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100">
          ← Customers
        </Link>
        <h1 className="text-2xl font-bold mt-2 mb-1">{customer.email}</h1>
        <p className="text-sm text-zinc-600 dark:text-zinc-400 mb-6">
          {customer.id} · plan {customer.plan_id}
        </p>

        <section aria-labelledby="usage-heading" className="border border-zinc-200 dark:border-zinc-700 rounded-lg p-4 mb-6">
          <h2 id="usage-heading" className="font-semibold mb-1">
            Usage
          </h2>
          <p className="text-sm text-zinc-500 dark:text-zinc-400 mb-4">
            {new Date(usage.period_start).toLocaleDateString()} – {new Date(usage.period_end).toLocaleDateString()} ·{" "}
            {usage.total_calls} calls · {usage.total_units} units
          </p>
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left">
                <tr>
                  <th className="px-3 py-2">Operation</th>
                  <th className="px-3 py-2">Calls</th>
                  <th className="px-3 py-2">Units</th>
                </tr>
              </thead>
              <tbody>
                {usage.breakdown.map((row) => (
                  <tr key={row.operation} className="border-t border-zinc-200 dark:border-zinc-700">
                    <td className="px-3 py-2">{row.operation}</td>
                    <td className="px-3 py-2">{row.calls}</td>
                    <td className="px-3 py-2">{row.total_units}</td>
                  </tr>
                ))}
                {usage.breakdown.length === 0 && (
                  <tr>
                    <td className="px-3 py-4 text-zinc-500" colSpan={3}>
                      No usage in this period.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>
        </section>

        <Link
          href={`/operator/audit?customer_id=${customer.id}`}
          className="text-sm underline text-zinc-600 dark:text-zinc-400"
        >
          View audit events for this customer →
        </Link>
      </div>
    </main>
  );
}
