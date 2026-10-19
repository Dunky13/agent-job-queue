// Rebuild with: esbuild entry.mjs --bundle --minify --format=esm --platform=browser --outfile=zod.mjs
// Requires zod 4.6.7 and esbuild 0.25.12 at build time only.
import { object, string, number, boolean, array, literal, iso, config } from "zod";
// The dashboard uses a strict CSP with no unsafe-eval. Interpret schemas directly.
config({ jitless: true });
export const z = { object, string, number, boolean, array, literal, iso };
