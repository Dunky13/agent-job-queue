import js from "@eslint/js";
import globals from "globals";
import { defineConfig } from "eslint/config";

export default defineConfig([
  { ignores: ["remote/static/vendor/**", "node_modules/**", "test-results/**", "playwright-report/**"] },
  js.configs.recommended,
  {
    files: ["remote/static/app.js", "remote/static/profiles.js", "remote/static/profile-selection.js", "remote/static/schema.mjs"],
    languageOptions: { globals: globals.browser },
  },
  {
    files: ["**/*.mjs"],
    languageOptions: { globals: globals.node },
  },
  {
    files: ["remote/tests/browser/*.mjs"],
    languageOptions: { globals: globals.browser },
  },
]);
