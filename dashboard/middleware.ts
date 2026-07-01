import NextAuth from "next-auth";
import type { NextFetchEvent, NextRequest } from "next/server";
import authConfig from "./auth.config";
import { NextResponse } from "next/server";
import { ALLOWED_ORIGIN, getOperatorToken } from "@/lib/env";
import { OPERATOR_SESSION_COOKIE, verifyOperatorSession } from "@/lib/operator/session";

const { auth } = NextAuth(authConfig);

// Operator paths use a distinct session (a cookie issued only after presenting
// OPERATOR_TOKEN at /operator/login) instead of the customer NextAuth session, so
// a normal customer's dashboard login never grants /operator/* access. The login
// page itself (and the server action it posts to, which Next.js routes to the
// same /operator/login URL) is excluded so it's reachable without a session.
async function handleOperatorRequest(req: NextRequest): Promise<Response> {
  const { pathname } = req.nextUrl;
  if (pathname === "/operator/login") {
    return NextResponse.next();
  }

  const authorized = await verifyOperatorSession(req.cookies.get(OPERATOR_SESSION_COOKIE)?.value, getOperatorToken());
  if (!authorized) {
    if (pathname.startsWith("/api/")) {
      return new Response(JSON.stringify({ error: "Unauthorized" }), {
        status: 401,
        headers: { "content-type": "application/json", "cache-control": "no-store" },
      });
    }
    return Response.redirect(new URL("/operator/login", req.nextUrl.origin));
  }
  return NextResponse.next();
}

const customerAuthMiddleware = auth((req, _event: NextFetchEvent) => {
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
  // Only set when absent: rotating on every load would invalidate tokens for
  // in-flight requests and multi-tab sessions (a new tab navigating rotates the
  // cookie, breaking the original tab's pending button click). The SameSite=Strict
  // attribute is the primary CSRF guard; the double-submit token is defense-in-depth.
  // Not needed on API sub-routes — the cookie is already set from the preceding page load.
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

export default function middleware(req: NextRequest, event: NextFetchEvent) {
  const { pathname } = req.nextUrl;
  if (pathname.startsWith("/operator") || pathname.startsWith("/api/operator")) {
    return handleOperatorRequest(req);
  }
  return customerAuthMiddleware(req, event);
}

export const config = {
  matcher: ["/dashboard/:path*", "/api/keys/:path*", "/api/usage", "/operator/:path*", "/api/operator/:path*"],
};
