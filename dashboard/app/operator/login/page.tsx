import { operatorLogin } from "../actions";

export default async function OperatorLoginPage({
  searchParams,
}: {
  searchParams: Promise<{ error?: string }>;
}) {
  const { error } = await searchParams;

  return (
    <main id="main-content" className="min-h-screen p-8 flex flex-col items-center justify-center">
      <div className="w-full max-w-sm border border-zinc-200 dark:border-zinc-700 rounded-lg p-6">
        <h1 className="text-2xl font-bold mb-1">Operator console</h1>
        <p className="text-sm text-zinc-600 dark:text-zinc-400 mb-5">
          Enter the operator token to view the read-only admin console.
        </p>
        {error && (
          <p className="text-sm text-red-600 dark:text-red-400 mb-3" role="alert">
            Invalid token.
          </p>
        )}
        <form action={operatorLogin} aria-label="Operator sign in">
          <label htmlFor="token" className="visually-hidden">
            Operator token
          </label>
          <input
            id="token"
            type="password"
            name="token"
            placeholder="Operator token"
            required
            autoComplete="off"
            aria-required="true"
            className="w-full px-3 py-2 border border-zinc-300 dark:border-zinc-600 rounded bg-transparent mb-3 text-zinc-900 dark:text-zinc-100 placeholder:text-zinc-400 dark:placeholder:text-zinc-500"
          />
          <button
            type="submit"
            className="w-full px-3 py-2 rounded bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900 font-medium"
          >
            Sign in
          </button>
        </form>
      </div>
    </main>
  );
}
