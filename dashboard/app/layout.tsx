import type { Metadata } from "next";
import type { ReactNode } from "react";
import Link from "next/link";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
});

export const metadata: Metadata = {
  title: "Pulse | Observability",
  description: "Host metrics and anomaly alert episodes",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html
      lang="en"
      className={`${geistSans.variable} ${geistMono.variable} h-full antialiased`}
    >
      <body className="min-h-full flex flex-col">
        <nav className="border-b border-zinc-200 dark:border-zinc-800">
          <div className="mx-auto flex max-w-5xl gap-5 px-6 py-3 text-sm">
            <Link href="/" className="font-semibold hover:underline">
              Pulse
            </Link>
            <Link href="/alerts" className="text-zinc-600 hover:underline dark:text-zinc-300">
              Alerts
            </Link>
          </div>
        </nav>
        {children}
      </body>
    </html>
  );
}
