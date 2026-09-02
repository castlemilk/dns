import type { NextConfig } from "next";

const securityHeaders = [
  { key: "Cross-Origin-Opener-Policy", value: "same-origin" },
  { key: "Cross-Origin-Resource-Policy", value: "same-origin" },
  { key: "Permissions-Policy", value: "camera=(), geolocation=(), microphone=()" },
  { key: "Referrer-Policy", value: "no-referrer" },
  { key: "Strict-Transport-Security", value: "max-age=31536000" },
  { key: "X-Content-Type-Options", value: "nosniff" },
  { key: "X-Frame-Options", value: "DENY" },
  { key: "X-Robots-Tag", value: "noindex, nofollow" },
];

const imageOptimizerRuntimeExcludes = [
  "**/node_modules/.pnpm/@img+*/**/*",
  "**/node_modules/.pnpm/sharp@*/**/*",
  "**/node_modules/@img{,/**/*}",
  "**/node_modules/sharp{,/**/*}",
];

if (process.env.NODE_ENV === "production") {
  securityHeaders.push({
    key: "Content-Security-Policy",
    value: [
      "base-uri 'self'",
      "connect-src 'self'",
      "default-src 'self'",
      "font-src 'self'",
      "form-action 'self'",
      "frame-ancestors 'none'",
      "img-src 'self' data:",
      "object-src 'none'",
      "script-src 'self' 'unsafe-inline'",
      "style-src 'self' 'unsafe-inline'",
    ].join("; "),
  });
}

const nextConfig: NextConfig = {
  images: {
    // This application only serves its checked-in SVG mark. Disabling the
    // optimizer keeps sharp and its native libvips payload out of the runtime.
    unoptimized: true,
  },
  output: "standalone",
  outputFileTracingExcludes: {
    // `next-server` is the documented special trace key for the shared
    // standalone server; `/*` also covers files traced per application route.
    "next-server": imageOptimizerRuntimeExcludes,
    "/*": imageOptimizerRuntimeExcludes,
  },
  poweredByHeader: false,
  async headers() {
    return [
      {
        source: "/:path*",
        headers: securityHeaders,
      },
    ];
  },
};

export default nextConfig;
