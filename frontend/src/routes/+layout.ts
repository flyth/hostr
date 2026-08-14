// Pure SPA: no server rendering, no prerendered routes. The Go binary serves
// index.html for every unmatched path and the router takes it from there.
export const ssr = false;
export const prerender = false;
export const trailingSlash = 'never';
