const USER = 'anyaer007';
const DEFAULT_BUCKET = 'image';

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const path = url.pathname;

    const parts = path.split('/').filter(p => p);
    if (parts.length < 2) {
      return new Response("Invalid path format. Use: /folder/filename or /bucket/folder/filename", { status: 400 });
    }

    let bucket, filename;
    if (parts.length >= 3) {
      bucket = parts[0];
      filename = parts.slice(1).join('/');
    } else {
      bucket = DEFAULT_BUCKET;
      filename = parts.join('/');
    }

    const hfUrl = `https://huggingface.co/buckets/${USER}/${bucket}/resolve/${filename}`;

    try {
      const response = await fetch(hfUrl, {
        method: request.method,
        headers: request.headers,
        redirect: 'follow'
      });

      const newResponse = new Response(response.body, response);
      newResponse.headers.set('Access-Control-Allow-Origin', '*');
      newResponse.headers.set('X-Proxy-By', 'Cloudflare-Workers-HF-Proxy');
      return newResponse;
    } catch (error) {
      return new Response('Proxy error: ' + error.message, { status: 502 });
    }
  },
};