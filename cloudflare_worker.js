const HF_REPO_ID = '';
const HF_TYPE = 'datasets';
const HF_BRANCH = 'main';

function getConfig(env) {
  const repoId = (env.HF_REPO_ID || HF_REPO_ID || '').trim();
  const repoType = (env.HF_TYPE || HF_TYPE || 'datasets').trim();
  const branch = (env.HF_BRANCH || HF_BRANCH || 'main').trim();

  if (!repoId || !repoId.includes('/')) {
    throw new Error('Worker misconfigured: set HF_REPO_ID to user/repo.');
  }

  const [user, repoName] = repoId.split('/', 2);
  return { user, repoName, repoType, branch };
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

export default {
  async fetch(request, env, ctx) {
    try {
      const { user, repoName, repoType, branch } = getConfig(env);
      const url = new URL(request.url);
      const parts = url.pathname.split('/').filter(Boolean).map(safeDecode);

      if (parts.length < 2) {
        return new Response('Invalid path format. Use: /folder/file', { status: 400 });
      }

      const encodedPath = encodePath(parts.join('/'));
      const hfUrl = `https://huggingface.co/${repoType}/${encodeURIComponent(user)}/${encodeURIComponent(repoName)}/resolve/${encodeURIComponent(branch)}/${encodedPath}`;
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
