import { defineConfig, globalIgnores } from "eslint/config";
import nextVitals from "eslint-config-next/core-web-vitals";
import nextTs from "eslint-config-next/typescript";

const eslintConfig = defineConfig([
  ...nextVitals,
  ...nextTs,
  // Override default ignores of eslint-config-next.
  globalIgnores([
    // Default ignores of eslint-config-next:
    ".next/**",
    "out/**",
    "build/**",
    "gen/**",
    "next-env.d.ts",
  ]),
  {
    rules: {
      "no-restricted-imports": [
        "error",
        {
          paths: [
            {
              name: "@/lib/sample-zone",
              message:
                "Sample data may only be rendered by components/marketing/product-frame.tsx.",
            },
            {
              name: "@/lib/sample-platform",
              message:
                "Sample data may only be rendered by components/marketing/product-frame.tsx.",
            },
          ],
        },
      ],
    },
  },
  {
    files: ["components/marketing/product-frame.tsx"],
    rules: {
      "no-restricted-imports": "off",
    },
  },
]);

export default eslintConfig;
