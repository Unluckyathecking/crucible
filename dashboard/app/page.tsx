import Link from "next/link";

export default function Home() {
  return (
    <main className="min-h-screen p-8 flex flex-col items-center justify-center gap-6">
      <h1 className="text-5xl font-bold tracking-tight">Crucible</h1>
      <p className="text-lg text-zinc-500 text-center max-w-md">
        A metered API platform. Clone the repo, adapt one worker, ship a new product.
      </p>
      <div className="flex gap-3">
        <Link
          href="/login"
          className="px-5 py-2 bg-zinc-900 text-white rounded hover:bg-zinc-700 transition"
        >
          Sign in
        </Link>
      </div>
    </main>
  );
}
