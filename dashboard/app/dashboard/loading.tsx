export default function DashboardLoading() {
  return (
    <main className="min-h-screen p-8 max-w-3xl mx-auto">
      <header className="flex justify-between items-center mb-8">
        <div className="h-8 w-36 rounded bg-zinc-200 animate-pulse dark:bg-zinc-700" />
        <div className="h-4 w-16 rounded bg-zinc-200 animate-pulse dark:bg-zinc-700" />
      </header>

      <section className="border border-zinc-200 rounded-lg p-5 mb-6 dark:border-zinc-700">
        <div className="h-3 w-24 rounded bg-zinc-200 animate-pulse dark:bg-zinc-700" />
        <div className="h-5 w-48 rounded bg-zinc-200 animate-pulse mt-2 dark:bg-zinc-700" />
        <div className="h-3 w-12 rounded bg-zinc-200 animate-pulse mt-3 dark:bg-zinc-700" />
        <div className="h-5 w-20 rounded bg-zinc-200 animate-pulse mt-2 dark:bg-zinc-700" />
      </section>

      <section className="border border-zinc-200 rounded-lg p-5 mb-6 dark:border-zinc-700">
        <div className="flex justify-between items-center mb-3">
          <div className="h-6 w-20 rounded bg-zinc-200 animate-pulse dark:bg-zinc-700" />
          <div className="h-8 w-24 rounded bg-zinc-200 animate-pulse dark:bg-zinc-700" />
        </div>
        <div className="space-y-2">
          <div className="h-4 w-full rounded bg-zinc-200 animate-pulse dark:bg-zinc-700" />
          <div className="h-4 w-3/4 rounded bg-zinc-200 animate-pulse dark:bg-zinc-700" />
        </div>
      </section>

      <section className="border border-zinc-200 rounded-lg p-5 dark:border-zinc-700">
        <div className="h-6 w-40 rounded bg-zinc-200 animate-pulse mb-3 dark:bg-zinc-700" />
        <div className="h-10 w-28 rounded bg-zinc-200 animate-pulse dark:bg-zinc-700" />
        <div className="h-3 w-24 rounded bg-zinc-200 animate-pulse mt-2 dark:bg-zinc-700" />
      </section>
    </main>
  );
}
