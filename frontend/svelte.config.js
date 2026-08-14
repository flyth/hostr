import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
const config = {
  preprocess: vitePreprocess(),
  compilerOptions: {
    runes: ({ filename }) =>
      filename.split(/[/\\]/).includes('node_modules') ? undefined : true
  },
  kit: {
    // Static SPA. `host` embeds this build directory and serves index.html for
    // any unmatched path, so client-side routing survives a deep link.
    adapter: adapter({
      pages: 'build',
      assets: 'build',
      fallback: 'index.html',
      precompress: false,
      strict: false
    }),
    paths: { base: '', relative: false },
    prerender: { entries: [] },
    // Hash mode emits a <meta> CSP covering SvelteKit's own inline bootstrap,
    // so the control panel ships a real policy without the server having to
    // mint nonces per request.
    csp: {
      mode: 'hash',
      directives: {
        'default-src': ['self'],
        'script-src': ['self'],
        'style-src': ['self', 'unsafe-inline'],
        'img-src': ['self', 'data:'],
        'connect-src': ['self'],
        'base-uri': ['self'],
        'form-action': ['self'],
        'frame-ancestors': ['none'],
        'object-src': ['none']
      }
    }
  }
};

export default config;
