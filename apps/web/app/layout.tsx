import type { Metadata } from "next";
import { IBM_Plex_Mono, IBM_Plex_Sans } from "next/font/google";

import { cn } from "@/lib/utils";
import "./globals.css";

const plexSans = IBM_Plex_Sans({
  subsets: ["latin"],
  weight: ["400", "500", "600"],
  display: "swap",
  variable: "--font-plex-sans",
});

const plexMono = IBM_Plex_Mono({
  subsets: ["latin"],
  weight: ["400", "500"],
  display: "swap",
  variable: "--font-plex-mono",
});

export const metadata: Metadata = {
  title: {
    default: "Deep Hosting",
    template: "%s · Deep Hosting",
  },
  description:
    "Web hosting, email and authoritative DNS for your domain — one console, no zone files.",
};

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html
      lang="en"
      className={cn(
        "dark h-full antialiased",
        plexSans.variable,
        plexMono.variable,
      )}
    >
      <body className="flex min-h-full flex-col bg-background">{children}</body>
    </html>
  );
}
