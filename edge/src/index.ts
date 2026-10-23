export interface Env {
  RADIO_AUDIO: R2Bucket;
  SESSION_SEED?: string;
}

const STATIC_ASSETS = new Set([
  "/htmx.min.js",
  "/player-adapter.js",
  "/station-store.js",
  "/manifest.json",
  "/icon.svg",
]);

const VIDEO_ID_RE = /^\/api\/stream\/([A-Za-z0-9_-]{11})$/;

export default {
  async fetch(request: Request, env: Env, ctx: ExecutionContext): Promise<Response> {
    const url = new URL(request.url);
    const method = request.method.toUpperCase();

    // 1. Edge-cache static assets with stale-while-revalidate
    if ((method === "GET" || method === "HEAD") && STATIC_ASSETS.has(url.pathname)) {
      return serveEdgeCachedAsset(request, ctx);
    }

    // 2. Serve cached audio tracks directly from Cloudflare R2 at the edge
    const streamMatch = VIDEO_ID_RE.exec(url.pathname);
    if (streamMatch && (method === "GET" || method === "HEAD")) {
      const videoId = streamMatch[1];
      const authorized = await isSessionAuthorized(request, env.SESSION_SEED);
      if (authorized) {
        return serveOrFallbackAudio(request, env, url, videoId, method);
      }
    }

    // 3. Pass all dynamic HTMX/API/OAuth routes through to tunnel origin
    return fetch(request);
  },
};

async function serveEdgeCachedAsset(request: Request, ctx: ExecutionContext): Promise<Response> {
  const cache = caches.default;
  const cacheKey = new Request(request.url, { method: "GET" });
  const cached = await cache.match(cacheKey);
  if (cached) {
    const headers = new Headers(cached.headers);
    headers.set("X-Radio-Edge", "ASSET-HIT");
    return new Response(request.method === "HEAD" ? null : cached.body, {
      status: cached.status,
      headers,
    });
  }

  const originResp = await fetch(request);
  if (originResp.status === 200 && request.method === "GET") {
    const headers = new Headers(originResp.headers);
    headers.set("Cache-Control", "public, max-age=300, stale-while-revalidate=86400");
    headers.set("X-Radio-Edge", "ASSET-MISS");
    const toCache = new Response(originResp.body, {
      status: 200,
      headers,
    });
    ctx.waitUntil(cache.put(cacheKey, toCache.clone()));
    return toCache;
  }
  return originResp;
}

async function serveOrFallbackAudio(
  request: Request,
  env: Env,
  url: URL,
  videoId: string,
  method: string
): Promise<Response> {
  const key = `tracks/${videoId}.m4a`;

  // Prefetch probe: if already in R2, short-circuit at the edge with zero origin traffic
  if (url.searchParams.get("prefetch") === "1") {
    const headObj = await env.RADIO_AUDIO.head(key);
    if (headObj) {
      return new Response(null, {
        status: 204,
        headers: {
          "X-Radio-Edge": "R2-PREFETCH-HIT",
          "Cache-Control": "no-store",
        },
      });
    }
    return fetch(request);
  }

  // HEAD probe for audio element metadata
  if (method === "HEAD") {
    const headObj = await env.RADIO_AUDIO.head(key);
    if (headObj) {
      return new Response(null, {
        status: 200,
        headers: buildAudioHeaders(headObj.size, headObj.httpMetadata?.contentType, "R2-HIT"),
      });
    }
    return fetch(request);
  }

  // Parse HTTP Range header if present (required by iOS Safari / mobile WebKit)
  const rangeHeader = request.headers.get("Range");
  if (rangeHeader) {
    const headObj = await env.RADIO_AUDIO.head(key);
    if (!headObj) {
      return fetch(request);
    }
    const total = headObj.size;
    const parsed = parseByteRange(rangeHeader, total);
    if (!parsed) {
      const headers = buildAudioHeaders(total, headObj.httpMetadata?.contentType, "R2-HIT");
      headers.set("Content-Range", `bytes */${total}`);
      return new Response(null, { status: 416, headers });
    }
    const { start, end } = parsed;
    const length = end - start + 1;
    const obj = await env.RADIO_AUDIO.get(key, {
      range: { offset: start, length },
    });
    if (!obj || !("body" in obj)) {
      return fetch(request);
    }
    const headers = buildAudioHeaders(length, obj.httpMetadata?.contentType, "R2-HIT");
    headers.set("Content-Range", `bytes ${start}-${end}/${total}`);
    return new Response(obj.body, {
      status: 206,
      headers,
    });
  }

  // Full GET request (no Range header)
  const obj = await env.RADIO_AUDIO.get(key);
  if (obj && "body" in obj) {
    const headers = buildAudioHeaders(obj.size, obj.httpMetadata?.contentType, "R2-HIT");
    return new Response(obj.body, {
      status: 200,
      headers,
    });
  }

  // R2 miss: fall back to origin tunnel (which streams immediately and uploads to R2 in background)
  return fetch(request);
}

function buildAudioHeaders(contentLength: number, contentType?: string, edgeStatus = "R2-HIT"): Headers {
  const headers = new Headers();
  headers.set("Content-Type", contentType || "audio/mp4");
  headers.set("Accept-Ranges", "bytes");
  headers.set("Content-Length", String(contentLength));
  headers.set("Cache-Control", "public, max-age=31536000, immutable");
  headers.set("X-Radio-Edge", edgeStatus);
  return headers;
}

function parseByteRange(rangeHeader: string, size: number): { start: number; end: number } | null {
  if (!rangeHeader.startsWith("bytes=") || size <= 0) {
    return null;
  }
  const spec = rangeHeader.slice(6).split(",")[0].trim();
  const dash = spec.indexOf("-");
  if (dash < 0) {
    return null;
  }
  const startStr = spec.slice(0, dash).trim();
  const endStr = spec.slice(dash + 1).trim();

  let start: number;
  let end: number;
  if (startStr === "") {
    const suffix = parseInt(endStr, 10);
    if (isNaN(suffix) || suffix <= 0) {
      return null;
    }
    start = Math.max(0, size - suffix);
    end = size - 1;
  } else {
    start = parseInt(startStr, 10);
    if (isNaN(start) || start < 0 || start >= size) {
      return null;
    }
    if (endStr === "") {
      end = size - 1;
    } else {
      end = parseInt(endStr, 10);
      if (isNaN(end) || end < start) {
        return null;
      }
      if (end >= size) {
        end = size - 1;
      }
    }
  }
  return { start, end };
}

async function isSessionAuthorized(request: Request, seed?: string): Promise<boolean> {
  const cookieHeader = request.headers.get("Cookie") || "";
  const token = extractCookie(cookieHeader, "radio_session");
  if (!token) {
    return false;
  }
  const dot = token.indexOf(".");
  if (dot <= 0) {
    return false;
  }
  const payloadB64 = token.slice(0, dot);
  const sigHex = token.slice(dot + 1);

  // If SESSION_SEED secret is bound, verify the exact HMAC-SHA256 signature at the edge
  if (seed) {
    const enc = new TextEncoder();
    const secretHash = await crypto.subtle.digest(
      "SHA-256",
      enc.encode(`radio-station-session-v2:${seed}`)
    );
    const key = await crypto.subtle.importKey(
      "raw",
      secretHash,
      { name: "HMAC", hash: "SHA-256" },
      false,
      [" sign", "verify"].map((s) => s.trim()) as KeyUsage[]
    );
    const sigBytes = hexToBytes(sigHex);
    if (!sigBytes) {
      return false;
    }
    const valid = await crypto.subtle.verify("HMAC", key, sigBytes, enc.encode(payloadB64));
    if (!valid) {
      return false;
    }
  }

  try {
    const decoded = atob(payloadB64.replace(/-/g, "+").replace(/_/g, "/"));
    const parts = decoded.split("|");
    if (parts.length !== 3) {
      return false;
    }
    const exp = parseInt(parts[2], 10);
    if (isNaN(exp) || Math.floor(Date.now() / 1000) > exp) {
      return false;
    }
    return true;
  } catch {
    return false;
  }
}

function extractCookie(cookieHeader: string, name: string): string | null {
  const parts = cookieHeader.split(";");
  for (const part of parts) {
    const eq = part.indexOf("=");
    if (eq > 0) {
      const k = part.slice(0, eq).trim();
      if (k === name) {
        return part.slice(eq + 1).trim();
      }
    }
  }
  return null;
}

function hexToBytes(hex: string): Uint8Array | null {
  if (hex.length % 2 !== 0) {
    return null;
  }
  const bytes = new Uint8Array(hex.length / 2);
  for (let i = 0; i < hex.length; i += 2) {
    const byte = parseInt(hex.slice(i, i + 2), 16);
    if (isNaN(byte)) {
      return null;
    }
    bytes[i / 2] = byte;
  }
  return bytes;
}
