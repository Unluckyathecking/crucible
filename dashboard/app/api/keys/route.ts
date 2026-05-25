import { auth } from "@/auth";
import { ensureCustomer, insertApiKey } from "@/lib/db";
import { generateKey, hashKey } from "@/lib/keys";

export async function POST(request: Request): Promise<Response> {
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
    const body = (await request.json()) as { name?: string };
    name = (body.name || "").trim();
  } else {
    const formData = await request.formData();
    name = (formData.get("name") as string | undefined || "").trim();
  }

  // Validate name: length and allowed characters
  if (name.length === 0) {
    return new Response("Name is required", { status: 400 });
  }
  if (name.length > 64) {
    return new Response("Name must be 64 characters or fewer", { status: 400 });
  }
  // Whitelist: alphanumeric, spaces, hyphens, underscores, periods, commas
  const validName = /^[a-zA-Z0-9 _.,-]+$/.test(name);
  if (!validName) {
    return new Response("Name contains invalid characters", { status: 400 });
  }

  const customer = await ensureCustomer(session.user.email);

  // Retry on the rare prefix-collision (the unique partial index on active prefixes
  // catches the case). 3 attempts is way more than statistically needed.
  let full: string;
  let inserted = false;
  for (let attempt = 0; attempt < 3 && !inserted; attempt++) {
    const generated = generateKey(productPrefix);
    full = generated.full;
    const hash = hashKey(salt, generated.full);
    try {
      await insertApiKey(customer.id, generated.prefix, hash, name);
      inserted = true;
    } catch (e) {
      const code = (e as { code?: string }).code;
      if (code !== "23505") throw e; // 23505 = Postgres unique_violation
    }
  }
  if (!inserted) {
    return new Response("Failed to generate a unique key after 3 attempts", { status: 500 });
  }

  // Show the full key ONCE — minimal HTML so the user can copy it.
  // Full UX with one-time-secret modal lands in Sprint 5b.
  const html = `<!doctype html>
<html><head><meta charset="utf-8"><title>New API key</title>
<style>body{font-family:system-ui;padding:2rem;max-width:42rem;margin:auto}
code{display:block;padding:1rem;background:#f4f4f5;border-radius:.5rem;font-size:1rem;word-break:break-all;margin:1rem 0}
a{color:#18181b}</style>
</head><body>
<h1>Your new API key</h1>
<p>Copy it now — it won't be shown again.</p>
<code>${full!}</code>
<p><a href="/dashboard">&larr; Back to dashboard</a></p>
</body></html>`;

  return new Response(html, { headers: { "content-type": "text/html; charset=utf-8" } });
}
