const HF_USER = '';
const HF_DEFAULT_BUCKET = '';

function getConfig(env) {
  const user = (env.HF_USER || HF_USER || '').trim();
  const defaultBucket = (env.HF_DEFAULT_BUCKET || HF_DEFAULT_BUCKET || '').trim();

  if (!user || user === 'your_huggingface_username') {
    throw new Error('Worker misconfigured: set HF_USER to your Hugging Face username.');
  }

  return { user, defaultBucket };
}

function safeDecode(segment) {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

function encodePath(pathname) {
  return pathname
    .split('/')
    .filter(Boolean)
    .map(segment => encodeURIComponent(segment))
    .join('/');
}

function resolveTarget(pathname, defaultBucket) {
  const parts = pathname.split('/').filter(Boolean).map(safeDecode);
  if (parts.length < 2) {
    return null;
  }

  return {
    bucket: parts[0],
    filename: parts.slice(1).join('/'),
  };
}

export default {
  async fetch(request, env, ctx) {
    try {
      const { user, defaultBucket } = getConfig(env);
      const url = new URL(request.url);
      const target = resolveTarget(url.pathname, defaultBucket);

      if (!target) {
        return new Response('Invalid path format. Use: /bucket/folder/file, or configure HF_DEFAULT_BUCKET to allow /folder/file.', { status: 400 });
      }

      const encodedBucket = encodeURIComponent(target.bucket);
      const encodedFilename = encodePath(target.filename);
      const hfUrl = `https://huggingface.co/buckets/${user}/${encodedBucket}/resolve/${encodedFilename}`;
      const cacheKey = new Request(hfUrl, request);
      const cache = caches.default;

      let response = await cache.match(cacheKey);
      if (response) {
        response = new Response(response.body, response);
        response.headers.set('X-Cache-Status', 'HIT');
        response.headers.set('Access-Control-Allow-Origin', '*');
        return response;
      }

      const upstreamHeaders = new Headers();
      const range = request.headers.get('Range');
      const accept = request.headers.get('Accept');
      if (range) upstreamHeaders.set('Range', range);
      if (accept) upstreamHeaders.set('Accept', accept);

      response = await fetch(hfUrl, {
        method: request.method,
        headers: upstreamHeaders,
        redirect: 'follow',
      });

      if (response.status === 200 && request.method === 'GET') {
        ctx.waitUntil(cache.put(cacheKey, response.clone()));
      }

      const proxied = new Response(response.body, response);
      proxied.headers.set('Access-Control-Allow-Origin', '*');
      proxied.headers.set('X-Cache-Status', 'MISS');
      proxied.headers.set('X-Proxy-By', 'Cloudflare-Workers-HF-Proxy');
      return proxied;
    } catch (error) {
      return new Response(error.message || 'Worker configuration error', { status: 500 });
    }
  },
};
