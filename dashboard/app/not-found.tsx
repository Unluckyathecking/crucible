import Link from "next/link";

export default function NotFound() {
  return (
    <main className="min-h-screen p-8 flex flex-col items-center justify-center">
      <div className="text-center">
        <h1 className="text-5xl font-bold tracking-tight mb-2">404</h1>
        <p className="text-lg text-zinc-500 mb-6">Page not found.</p>
        <Link
          href="/"
          className="px-5 py-2 bg-zinc-900 text-white rounded hover:bg-zinc-700 transition inline-block"
        >
          Go home
        </Link>
      </div>
    </main>
  );
}
