export default function LoginLoading() {
  return (
    <main className="min-h-screen p-8 flex flex-col items-center justify-center">
      <div className="w-full max-w-sm border border-zinc-200 rounded-lg p-6 dark:border-zinc-700">
        <div className="h-7 w-48 rounded bg-zinc-200 animate-pulse mb-1 dark:bg-zinc-700" />
        <div className="h-4 w-56 rounded bg-zinc-200 animate-pulse mb-5 dark:bg-zinc-700" />
        <div className="h-10 w-full rounded bg-zinc-200 animate-pulse mb-3 dark:bg-zinc-700" />
        <div className="h-10 w-full rounded bg-zinc-200 animate-pulse dark:bg-zinc-700" />
      </div>
    </main>
  );
}
