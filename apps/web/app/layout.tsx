import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: {
    default: "Simple DNS",
    template: "%s · Simple DNS",
  },
  description: "A small, authoritative DNS control plane.",
};

export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    <html lang="en" className="dark h-full antialiased">
      <body className="flex min-h-full flex-col">{children}</body>
    </html>
  );
}
