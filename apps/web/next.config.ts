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
      // Loopback is here for one screen: /cli/authorize posts the operator token
      // to the callback `simple auth login` is listening on
      // (http://127.0.0.1:<ephemeral>/callback). Without it `connect-src 'self'`
      // blocks that POST in production and the CLI login can never complete.
      // Loopback is a potentially-trustworthy origin, so it is not mixed
      // content, and it can only ever reach the operator's own machine.
      "connect-src 'self' http://127.0.0.1:*",
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
