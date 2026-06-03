import Redis from "ioredis";

declare global {
  // eslint-disable-next-line no-var
  var _crucible_redis: Redis | undefined;
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

  if (!global._crucible_redis) {
    const redis = new Redis(url, {
      maxRetriesPerRequest: REDIS_MAX_RETRIES_PER_REQUEST,
      enableOfflineQueue: false,
    });
    redis.on("error", (err) => {
      console.error("Redis client error:", err.message);
    });
    global._crucible_redis = redis;
  }
  return global._crucible_redis;
}
