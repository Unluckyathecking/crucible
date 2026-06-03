import NextAuth from "next-auth";
import authConfig from "./auth.config";

const { auth } = NextAuth(authConfig);

export default auth((req) => {
  if (!req.auth) {
    // API routes need 401 JSON, not a 302 redirect to an HTML login page.
    // Use decoded pathname to avoid path traversal via encoded slashes.
    // Fall back to raw pathname if decoding fails (malformed UTF-8 like %E0%A0).
    let decodedPath: string;
    try {
      decodedPath = decodeURIComponent(req.nextUrl.pathname);
    } catch {
      decodedPath = req.nextUrl.pathname;
    }
    if (decodedPath.startsWith("/api/")) {
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
