/* eslint-disable @next/next/no-page-custom-font -- App Router root layout loads the approved console fonts globally. */
import type { Metadata, Viewport } from "next";
import { SessionProvider } from "../components/session";
import "./globals.css";

export const metadata: Metadata = {
  title: "VTX Operator Console",
  description: "Operational dashboard for VTX pipeline health, task runs, evidence, and audited controls",
};

export const viewport: Viewport = {
  colorScheme: "dark",
  width: "device-width",
  initialScale: 1,
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en" className="dark">
      <head>
        <link rel="preconnect" href="https://fonts.googleapis.com" />
        <link rel="preconnect" href="https://fonts.gstatic.com" crossOrigin="anonymous" />
        <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500;600&family=Plus+Jakarta+Sans:wght@400;500;600;700;800&display=swap" rel="stylesheet" />
      </head>
      <body className="min-h-screen bg-base text-slate-100 antialiased selection:bg-accent selection:text-white">
        <SessionProvider>{children}</SessionProvider>
      </body>
    </html>
  );
}
