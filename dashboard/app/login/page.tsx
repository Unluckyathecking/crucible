import { SubmitButton } from "./submit-button";
import { signInWithEmail, signInWithSSO } from "./actions";
import { ssoStatus } from "@/lib/sso";

export default function LoginPage() {
  // Server-side only: ssoStatus() returns just a boolean + display name. The
  // license key and its parsed contents never reach the client. In community mode
  // (no license) `enabled` is false and the page renders exactly as before.
  const { enabled: ssoEnabled, displayName: ssoName } = ssoStatus();

  return (
    <main id="main-content" className="min-h-screen p-8 flex flex-col items-center justify-center">
      <div className="w-full max-w-sm border border-zinc-200 dark:border-zinc-700 rounded-lg p-6">
        <h1 className="text-2xl font-bold mb-1">Sign in to Crucible</h1>
        <p className="text-sm text-zinc-600 dark:text-zinc-400 mb-5">We&apos;ll email you a magic link.</p>
        {ssoEnabled && (
          <>
            <form action={signInWithSSO} aria-label={`Sign in with ${ssoName}`}>
              <button
                type="submit"
                className="w-full px-3 py-2 border border-zinc-300 dark:border-zinc-600 rounded bg-transparent text-zinc-900 dark:text-zinc-100 hover:bg-zinc-100 dark:hover:bg-zinc-800 transition mb-4"
              >
                Continue with {ssoName}
              </button>
            </form>
            <div className="flex items-center gap-3 mb-4" aria-hidden="true">
              <span className="h-px flex-1 bg-zinc-200 dark:bg-zinc-700" />
              <span className="text-xs text-zinc-500 dark:text-zinc-400">or</span>
              <span className="h-px flex-1 bg-zinc-200 dark:bg-zinc-700" />
            </div>
          </>
        )}
        <form
          action={signInWithEmail}
          aria-label="Sign in with email"
        >
          <label htmlFor="email" className="visually-hidden">
            Email address
          </label>
          <input
            id="email"
            type="email"
            name="email"
            placeholder="you@example.com"
            required
            autoComplete="email"
            aria-required="true"
            className="w-full px-3 py-2 border border-zinc-300 dark:border-zinc-600 rounded bg-transparent mb-3 text-zinc-900 dark:text-zinc-100 placeholder:text-zinc-400 dark:placeholder:text-zinc-500"
          />
          <SubmitButton />
        </form>
        <p className="text-xs text-zinc-600 dark:text-zinc-400 mt-4">
          In dev without RESEND_API_KEY, the link is logged to the dashboard console — copy it from there.
        </p>
      </div>
    </main>
  );
}
