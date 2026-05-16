import { signIn } from "@/auth";

export default function LoginPage() {
  return (
    <main className="min-h-screen p-8 flex flex-col items-center justify-center">
      <div className="w-full max-w-sm border border-zinc-200 rounded-lg p-6">
        <h1 className="text-2xl font-bold mb-1">Sign in to Crucible</h1>
        <p className="text-sm text-zinc-500 mb-5">We'll email you a magic link.</p>
        <form
          action={async (formData: FormData) => {
            "use server";
            await signIn("nodemailer", formData);
          }}
        >
          <input
            type="email"
            name="email"
            placeholder="you@example.com"
            required
            className="w-full px-3 py-2 border border-zinc-300 rounded bg-transparent mb-3"
          />
          <button
            type="submit"
            className="w-full px-3 py-2 bg-zinc-900 text-white rounded hover:bg-zinc-700 transition"
          >
            Send magic link
          </button>
        </form>
        <p className="text-xs text-zinc-500 mt-4">
          In dev without RESEND_API_KEY, the link is logged to the dashboard console — copy it from there.
        </p>
      </div>
    </main>
  );
}
