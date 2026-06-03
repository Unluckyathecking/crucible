import NextAuth from "next-auth";
import authConfig from "./auth.config";

const { auth } = NextAuth(authConfig);

export default auth((req) => {
  if (!req.auth) {
    // nextUrl.pathname is already decoded by Next.js — no double-decode needed.
    // API routes get 401 JSON; page routes redirect to login.
    if (req.nextUrl.pathname.toLowerCase().startsWith("/api/")) {
      return new Response(JSON.stringify({ error: "Unauthorized" }), {
        status: 401,
        headers: { "content-type": "application/json" },
      });
    }
    const url = new URL("/login", req.nextUrl.origin);
    return Response.redirect(url);
  }
});

export const config = {
  matcher: ["/dashboard/:path*", "/api/keys/:path*", "/api/usage", "/api/usage/", "/api/usage/:path*"],
};
