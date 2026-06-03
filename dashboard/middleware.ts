import NextAuth from "next-auth";
import authConfig from "./auth.config";

const { auth } = NextAuth(authConfig);

export default auth((req) => {
  if (!req.auth) {
    // API routes need 401 JSON, not a 302 redirect to an HTML login page.
    // Normalize repeated slashes (e.g. //api/usage) so the startsWith check
    // cannot be bypassed by double-slash paths that decodeURIComponent leaves intact.
    const normalizedPath = req.nextUrl.pathname.replace(/\/+/g, "/");
    if (normalizedPath.startsWith("/api/")) {
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
  matcher: ["/dashboard/:path*", "/api/keys/:path*", "/api/usage", "/api/usage/:path*"],
};
