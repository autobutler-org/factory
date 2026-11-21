import eslint from "@eslint/js";
import pluginVitest from "@vitest/eslint-plugin";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["node_modules/**"] },
  eslint.configs.recommended,
  tseslint.configs.recommended,
  reactHooks.configs.flat["recommended-latest"],
  { languageOptions: { globals: globals.browser } },
  { ...pluginVitest.configs.recommended, files: ["src/**/*.test.{ts,tsx}"] }
);
