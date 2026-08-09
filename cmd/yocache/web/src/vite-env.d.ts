/// <reference types="svelte" />
/// <reference types="vite/client" />

// Side-effect stylesheet imports (`import './app.css';`) are Vite's job at
// build time, not TypeScript's — declare the module here so tsc / the IDE
// stop flagging it, regardless of whether vite/client's ambient types are
// reachable through the current tsconfig `types`/`moduleResolution` combo.
declare module '*.css';
