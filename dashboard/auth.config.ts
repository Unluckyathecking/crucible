// Edge-safe NextAuth config — used by the Edge middleware so it never imports Node modules
// (pg, nodemailer, etc.). The full config in `auth.ts` extends this with the PG adapter and
// the real sendVerificationRequest implementation.
import type { NextAuthConfig } from "next-auth";

export default {
  session: { strategy: "jwt" },
  providers: [], // Real providers are declared in auth.ts; middleware only checks JWT presence.
  pages: { signIn: "/login" },
} satisfies NextAuthConfig;
