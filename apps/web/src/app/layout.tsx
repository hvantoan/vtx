import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "VTX Platform Cockpit | Autonomous Development Pipeline",
  description: "Real-time operations, orchestration health, and telemetry dashboard for VTX Platform",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" className="dark">
      <body className="min-h-screen bg-[#0b0f19] text-slate-100 antialiased selection:bg-blue-600 selection:text-white">
        {children}
      </body>
    </html>
  );
}
