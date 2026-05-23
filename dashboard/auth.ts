import NextAuth from "next-auth";
import Nodemailer from "next-auth/providers/nodemailer";
import PostgresAdapter from "@auth/pg-adapter";
import { Pool } from "pg";
import authConfig from "./auth.config";

const pool = new Pool({ connectionString: process.env.DATABASE_URL });

export const { handlers, auth, signIn, signOut } = NextAuth({
  ...authConfig,
  adapter: PostgresAdapter(pool),
  providers: [
    Nodemailer({
      // server unused — sendVerificationRequest handles delivery directly.
      server: { host: "localhost", port: 25, auth: { user: "noop", pass: "noop" } },
      from: process.env.EMAIL_FROM || "Crucible <onboarding@localhost>",
      sendVerificationRequest: async ({ identifier, url }) => {
        const apiKey = process.env.RESEND_API_KEY;
        if (!apiKey) {
          if (process.env.NODE_ENV === "development") {
            // Dev path: log the magic-link to console.
            console.log(
              `\n=== MAGIC LINK for ${identifier} ===\n${url}\n=====================================\n`,
            );
          }
          return;
        }
        const from = process.env.EMAIL_FROM || "Crucible <onboarding@resend.dev>";
        const res = await fetch("https://api.resend.com/emails", {
          method: "POST",
          headers: { Authorization: `Bearer ${apiKey}`, "Content-Type": "application/json" },
          body: JSON.stringify({
            from,
            to: identifier,
            subject: "Sign in to Crucible",
            html: `<p>Sign in with this link: <a href="${url}">${url}</a></p>`,
            text: `Sign in with this link: ${url}`,
          }),
        });
        if (!res.ok) {
          throw new Error(`Resend send failed: ${res.status}`);
        }
      },
    }),
  ],
});
