// Pure SPA: no server-side rendering, no prerendering. adapter-static emits
// the index.html fallback which the Go binary serves for every route.
export const ssr = false;
export const prerender = false;
