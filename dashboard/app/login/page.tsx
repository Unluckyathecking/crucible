import { signIn } from "@/auth";
import { SubmitButton } from "./submit-button";

export default function LoginPage() {
  return (
    <main id="main-content" className="min-h-screen p-8 flex flex-col items-center justify-center">
      <div className="w-full max-w-sm border border-zinc-200 dark:border-zinc-700 rounded-lg p-6">
        <h1 className="text-2xl font-bold mb-1">Sign in to Crucible</h1>
        <p className="text-sm text-zinc-600 dark:text-zinc-400 mb-5">We&apos;ll email you a magic link.</p>
        <form
          action={async (formData: FormData) => {
            "use server";
            await signIn("nodemailer", formData);
          }}
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
