import Redis from "ioredis";

declare global {
  // eslint-disable-next-line no-var
  var _crucible_redis: Redis | undefined;
  // eslint-disable-next-line no-var
  var _crucible_redis_url: string | undefined;
}

// Cap at 2 retries per command: enough to survive a transient blip without
// stalling the revocation path while the gateway continues serving the old cache.
const REDIS_MAX_RETRIES_PER_REQUEST = 2;

// Returns a shared Redis client if REDIS_URL is configured, or null if not.
// The dashboard uses Redis only for best-effort cache invalidation after key
// revocation (clearing auth:{prefix} so the gateway re-checks Postgres
// immediately rather than waiting for the 60-second TTL).
//
// A global singleton is used so the connection is established once and reused;
// creating a new client per call would leak TCP connections.
//
// enableOfflineQueue: false rejects commands immediately if Redis is not yet
// connected rather than accumulating them in an unbounded in-memory queue.
// For best-effort cache invalidation this is the right trade-off: a failed DEL
// means the key stays cached until TTL expiry (at most 60 s), which is
// acceptable. The alternative — an unbounded offline queue — risks OOM under a
// sustained Redis outage.
export function getRedis(): Redis | null {
  const url = process.env.REDIS_URL;
  if (!url) return null;

  if (!url.startsWith("redis://") && !url.startsWith("rediss://")) {
    console.error("REDIS_URL must start with redis:// or rediss://; skipping Redis client");
    return null;
  }

  // Recreate the client if REDIS_URL changed (e.g., between test runs or a config reload).
  // Without URL tracking, a changed REDIS_URL would silently return the stale client
  // created for the old address, causing cache invalidation to hit the wrong server.
  // Node.js runs on a single-threaded event loop; getRedis() is synchronous (no
  // await), so two callers cannot interleave here. No lock or atomic swap is needed.
  if (global._crucible_redis && global._crucible_redis_url !== url) {
    // Remove the error listener before disconnecting so the old client's socket-close
    // events don't produce spurious log noise after the client is replaced.
    global._crucible_redis.removeAllListeners("error");
    global._crucible_redis.disconnect();
    global._crucible_redis = undefined;
    global._crucible_redis_url = undefined;
  }

  if (!global._crucible_redis) {
    const redis = new Redis(url, {
      maxRetriesPerRequest: REDIS_MAX_RETRIES_PER_REQUEST,
      enableOfflineQueue: false,
    });
    redis.on("error", (err) => {
      console.error("Redis client error:", err);
    });
    global._crucible_redis = redis;
    global._crucible_redis_url = url;
  }
  return global._crucible_redis;
}
