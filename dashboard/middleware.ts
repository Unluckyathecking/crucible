import NextAuth from "next-auth";
import authConfig from "./auth.config";
import { NextResponse } from "next/server";
import { ALLOWED_ORIGIN } from "@/lib/env";

const { auth } = NextAuth(authConfig);

export default auth((req) => {
  if (!req.auth) {
    // nextUrl.pathname is already percent-decoded by Next.js.
    // API routes get 401 JSON; page routes redirect to login.
    if (req.nextUrl.pathname.startsWith("/api/")) {
      return new Response(JSON.stringify({ error: "Unauthorized" }), {
        status: 401,
        headers: { "content-type": "application/json", "cache-control": "no-store" },
      });
    }
    const url = new URL("/login", req.nextUrl.origin);
    return Response.redirect(url);
  }

  // Set CSRF double-submit cookie on dashboard page loads so client components
  // can read it and echo it as X-CSRF-Token on state-changing POST requests.
  // Not needed on API sub-routes — the cookie is already present from page load.
  if (!req.nextUrl.pathname.startsWith("/api/") && !req.cookies.get("__csrf")) {
    const token = crypto.randomUUID().replace(/-/g, "");
    const res = NextResponse.next();
    res.cookies.set("__csrf", token, {
      httpOnly: false,    // must be JS-readable for the double-submit pattern
      sameSite: "strict", // prevents cross-site cookie submission
      // Also set secure when ALLOWED_ORIGIN is https: handles TLS-terminating proxies
      // where NODE_ENV is not exactly "production" but the origin is HTTPS.
      secure: process.env.NODE_ENV === "production" || ALLOWED_ORIGIN.startsWith("https://"),
      path: "/",
      maxAge: 86400,      // 24 h; refreshed on next page load
    });
    return res;
  }
});

export const config = {
  matcher: ["/dashboard/:path*", "/api/keys/:path*", "/api/usage"],
};
