import Link from "next/link";
import { operatorLogout } from "../actions";

export function OperatorNav() {
  return (
    <header className="border-b border-zinc-200 dark:border-zinc-700 pb-3 mb-6 flex items-center justify-between">
      <nav className="flex items-center gap-4 text-sm" aria-label="Operator console">
        <Link href="/operator/customers" className="font-semibold text-zinc-900 dark:text-zinc-100">
          Customers
        </Link>
        <Link href="/operator/audit" className="text-zinc-600 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100">
          Audit log
        </Link>
        <Link href="/operator/plans" className="text-zinc-600 dark:text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100">
          Plans
        </Link>
      </nav>
      <form action={operatorLogout}>
        <button type="submit" className="text-sm text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100">
          Sign out
        </button>
      </form>
    </header>
  );
}
