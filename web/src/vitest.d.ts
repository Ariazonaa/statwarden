/**
 * Teaches `svelte-check` about the jest-dom matchers the tests use. The runtime
 * registration lives in ../vitest-setup.ts, which sits outside the tsconfig
 * include, so the `declare module 'vitest'` augmentation would otherwise never
 * reach the type-check program.
 */
import '@testing-library/jest-dom/vitest';
