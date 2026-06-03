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

  // Validate name
  if (name.length > 64) {
    return new Response("Name must be 64 characters or fewer", { status: 400 });
  }

  const customer = await ensureCustomer(session.user.email);

  // Retry on the rare prefix-collision (the unique partial index on active prefixes
  // catches the case). Three attempts is way more than statistically needed given
  // 15 random base32 chars (PrefixLen=24 minus 4-char product prefix = ~10^11 values).
  const MAX_KEY_GEN_ATTEMPTS = 3;
  let full = "";
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
      if (code !== "23505") throw e; // 23505 = Postgres unique_violation
    }
  }
  if (!inserted) {
    return new Response(`Failed to generate a unique key after ${MAX_KEY_GEN_ATTEMPTS} attempts`, { status: 500 });
  }

  // Return the full key ONCE as JSON — shown inline in the dashboard.
  // Never returned again; the client must display and copy from this response.
  return new Response(JSON.stringify({ key: full }), {
    headers: { "content-type": "application/json" },
  });
}
