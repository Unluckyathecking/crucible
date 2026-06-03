import { auth } from "@/auth";
import { ensureCustomer, insertApiKey } from "@/lib/db";
import { generateKey, hashKey } from "@/lib/keys";

const MAX_KEY_GEN_ATTEMPTS = 3;
const PG_UNIQUE_VIOLATION = "23505";

export async function POST(request: Request): Promise<Response> {
  let customerId: string | undefined;
  try {
    // Lightweight CSRF signal matching the DELETE route: custom headers require CORS
    // preflight on cross-origin requests. Defense-in-depth alongside SameSite cookies.
    const xrw = request.headers.get("X-Requested-With");
    if (!xrw || xrw.toLowerCase() !== "xmlhttprequest") {
      const safeHeader = xrw ? xrw.replace(/[^a-zA-Z0-9_-]/g, "").slice(0, 20) : "missing";
      console.warn("CSRF check failed for POST /api/keys", { header: safeHeader });
      return new Response("Forbidden", { status: 403 });
    }

    const session = await auth();
    if (!session?.user?.email) {
      return new Response("Unauthorized", { status: 401 });
    }

    const salt = process.env.API_KEY_HASH_SALT;
    if (!salt || salt.length < 32) {
      return new Response("API_KEY_HASH_SALT not configured (>= 32 bytes)", { status: 500 });
    }
    const productPrefix = process.env.API_KEY_PREFIX || "cru_";

    // Accept name from both FormData (server action) and JSON body
    let name = "";
    const contentType = request.headers.get("content-type") || "";
    if (contentType.includes("application/json")) {
      let body: unknown;
      try {
        body = await request.json();
      } catch (err) {
        console.error("POST /api/keys JSON parse failed:", err instanceof Error ? err.message : String(err));
        return new Response("Invalid JSON", { status: 400 });
      }
      const nameValue = (body as Record<string, unknown>).name;
      name = typeof nameValue === "string" ? nameValue.trim() : "";
    } else {
      const formData = await request.formData();
      name = (formData.get("name") as string | undefined || "").trim();
    }

    // Validate name
    if (name.length > 64) {
      return new Response("Name must be 64 characters or fewer", { status: 400 });
    }
    if (name.length > 0 && !/^[a-zA-Z0-9 _\-./]+$/.test(name)) {
      return new Response("Name contains invalid characters", { status: 400 });
    }

    const customer = await ensureCustomer(session.user.email);
    customerId = customer.id;

    // Retry on the rare prefix-collision (the unique partial index on active prefixes
    // catches the case). Three attempts is way more than statistically needed given
    // 15 random base32 chars of entropy (keyspace 32^15 ≈ 3.5×10^22; birthday-paradox
    // collision expected at ~1×10^11 active keys — far beyond any realistic deployment).
    let full: string | undefined;
    let inserted = false;
    for (let attempt = 0; attempt < MAX_KEY_GEN_ATTEMPTS && !inserted; attempt++) {
      const generated = generateKey(productPrefix);
      full = generated.full;
      const hash = hashKey(salt, generated.full);
      try {
        await insertApiKey(customer.id, generated.prefix, hash, name);
        inserted = true;
      } catch (e) {
        const code = (e as { code?: string }).code;
        if (code !== PG_UNIQUE_VIOLATION) throw e;
      }
    }
    if (!inserted || !full) {
      return new Response(`Failed to generate a unique key after ${MAX_KEY_GEN_ATTEMPTS} attempts`, { status: 500 });
    }

    // Return the full key ONCE as JSON — shown inline in the dashboard.
    // Never returned again; the client must display and copy from this response.
    return new Response(JSON.stringify({ key: full }), {
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    });
  } catch (err) {
    const errorId = crypto.randomUUID();
    console.error("POST /api/keys failed:", {
      errorId,
      customerId,
      error: err instanceof Error ? err.message : String(err),
    });
    return new Response("Internal server error", { status: 500 });
  }
}
