"use client";

export default function LoginError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return (
    <main className="min-h-screen p-8 flex flex-col items-center justify-center">
      <div className="w-full max-w-sm text-center">
        <div className="text-4xl mb-4">!</div>
        <h1 className="text-2xl font-bold mb-2">Something went wrong</h1>
        <p className="text-sm text-zinc-500 mb-6">
          The sign-in page failed to load. Please try again.
        </p>
        {error.digest && (
          <p className="text-xs font-mono text-zinc-400 mb-4">
            Error: {error.digest}
          </p>
        )}
        <button
          onClick={reset}
          className="px-5 py-2 bg-zinc-900 text-white rounded hover:bg-zinc-700 transition"
        >
          Try again
        </button>
      </div>
    </main>
  );
}
