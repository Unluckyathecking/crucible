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
  // getRedis() is fully synchronous (no await). The Node.js event loop is single-
  // threaded, so no two invocations can interleave within the same process — no lock
  // or atomic compare-and-swap is needed.
  //
  // Globals are cleared BEFORE disconnect() so any subsequent synchronous getRedis()
  // call (on a later event-loop tick) sees no stale client and creates a new one.
  // disconnect() triggers socket closure but does not await it; the old client object
  // is fully dereferenced from the module scope at this point.
  if (global._crucible_redis && global._crucible_redis_url !== url) {
    const oldRedis = global._crucible_redis;
    global._crucible_redis = undefined;
    global._crucible_redis_url = undefined;
    // Remove ALL listeners (not just "error") so no connect/ready/close callbacks
    // fire after the client is dereferenced and replaced.
    oldRedis.removeAllListeners();
    // quit() sends QUIT to Redis and waits for acknowledgement before closing;
    // disconnect() forces the socket closed immediately. Both are fire-and-forget
    // here (we already dereferenced the client above). Suppress shutdown errors.
    oldRedis.quit().catch((err) => {
      console.debug("redis quit during URL change:", err instanceof Error ? err.message : String(err));
    });
  }

  if (!global._crucible_redis) {
    const redis = new Redis(url, {
      maxRetriesPerRequest: REDIS_MAX_RETRIES_PER_REQUEST,
      enableOfflineQueue: false,
    });
    redis.on("error", (err) => {
      console.error("Redis client error:", err);
    });
    // Graceful shutdown: close the connection on process exit to avoid dangling
    // TCP connections (close-wait) on the Redis server under SIGTERM/SIGINT/beforeExit.
    // Guard checks that this client is still the current singleton before quitting —
    // a URL change may have already replaced it and called quit() on the old instance.
    const gracefulClose = () => {
      if (global._crucible_redis === redis) {
        redis.quit().catch(() => {});
      }
    };
    process.once("beforeExit", gracefulClose);
    process.once("SIGTERM", gracefulClose);
    process.once("SIGINT", gracefulClose);
    global._crucible_redis = redis;
    global._crucible_redis_url = url;
  }
  return global._crucible_redis;
}
