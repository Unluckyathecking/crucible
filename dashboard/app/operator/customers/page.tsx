import Link from "next/link";
import { OperatorNav } from "../_components/operator-nav";
import { Pagination } from "../_components/pagination";
import { listCustomers, listPlans } from "@/lib/operator/client";

export const dynamic = "force-dynamic";

const PER_PAGE = 20;

interface CustomersPageProps {
  searchParams: Promise<{ plan_id?: string; page?: string }>;
}

export default async function OperatorCustomersPage({ searchParams }: CustomersPageProps) {
  const params = await searchParams;
  const page = Math.max(1, Number(params.page) || 1);
  const planId = params.plan_id || undefined;

  const [customers, plans] = await Promise.all([
    listCustomers({ planId, page, perPage: PER_PAGE }),
    listPlans(),
  ]);

  return (
    <main id="main-content" className="min-h-screen px-4 py-6 sm:px-6 sm:py-8 md:px-8">
      <div className="mx-auto w-full max-w-5xl">
        <OperatorNav />
        <h1 className="text-2xl font-bold mb-4">Customers</h1>

        <form method="GET" className="flex items-center gap-3 mb-4 text-sm">
          <label htmlFor="plan_id" className="text-zinc-600 dark:text-zinc-400">
            Plan
          </label>
          <select
            id="plan_id"
            name="plan_id"
            defaultValue={planId ?? ""}
            className="px-2 py-1 border border-zinc-300 dark:border-zinc-600 rounded bg-transparent"
          >
            <option value="">All plans</option>
            {plans.map((plan) => (
              <option key={plan.id} value={plan.id}>
                {plan.display_name}
              </option>
            ))}
          </select>
          <button type="submit" className="px-3 py-1 border border-zinc-300 dark:border-zinc-600 rounded">
            Filter
          </button>
        </form>

        <div className="overflow-x-auto border border-zinc-200 dark:border-zinc-700 rounded-lg">
          <table className="w-full text-sm">
            <thead className="bg-zinc-50 dark:bg-zinc-800 text-left">
              <tr>
                <th className="px-3 py-2">Email</th>
                <th className="px-3 py-2">Plan</th>
                <th className="px-3 py-2">Created</th>
              </tr>
            </thead>
            <tbody>
              {customers.items.map((customer) => (
                <tr key={customer.id} className="border-t border-zinc-200 dark:border-zinc-700">
                  <td className="px-3 py-2">
                    <Link href={`/operator/customers/${customer.id}`} className="underline">
                      {customer.email}
                    </Link>
                  </td>
                  <td className="px-3 py-2">{customer.plan_id}</td>
                  <td className="px-3 py-2">{new Date(customer.created_at).toLocaleString()}</td>
                </tr>
              ))}
              {customers.items.length === 0 && (
                <tr>
                  <td className="px-3 py-4 text-zinc-500" colSpan={3}>
                    No customers found.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>

        <Pagination
          basePath="/operator/customers"
          page={page}
          perPage={PER_PAGE}
          total={customers.total}
          searchParams={params}
        />
      </div>
    </main>
  );
}
