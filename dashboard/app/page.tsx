"use client";

import { useEffect, useState } from "react";
import MetricChart, { type Point } from "./MetricChart";

const API_URL = process.env.NEXT_PUBLIC_PULSE_API ?? "http://localhost:8080";
const REFRESH_MS = 5000;
const RANGES = ["5m", "15m", "1h", "6h", "24h"];

export default function Home() {
  const [host, setHost] = useState(process.env.NEXT_PUBLIC_DEFAULT_HOST ?? "");
  const [range, setRange] = useState("15m");
  const [cpu, setCpu] = useState<Point[]>([]);
  const [mem, setMem] = useState<Point[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);

  useEffect(() => {
    if (!host) return;
    let cancelled = false;

    async function fetchMetric(name: string): Promise<Point[]> {
      const params = new URLSearchParams({ host, name, since: range });
      const res = await fetch(`${API_URL}/metrics?${params}`);
      if (!res.ok) {
        throw new Error(`${res.status}: ${await res.text()}`);
      }
      const data = await res.json();
      return data.points;
    }

    async function load() {
      try {
        const [cpuPoints, memPoints] = await Promise.all([
          fetchMetric("cpu.percent"),
          fetchMetric("mem.percent"),
        ]);
        if (cancelled) return;
        setCpu(cpuPoints);
        setMem(memPoints);
        setError(null);
        setLastUpdated(new Date());
      } catch (e) {
        if (!cancelled) {
          setError(e instanceof Error ? e.message : String(e));
        }
      }
    }

    load();
    const id = setInterval(load, REFRESH_MS);

    // Cleanup: stop polling when host/range changes or the page unmounts.
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [host, range]);

  return (
    <main className="mx-auto max-w-5xl p-6 space-y-6">
      <header className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-3xl font-bold">Pulse</h1>
          <p className="text-sm text-zinc-500">
            {lastUpdated
              ? `Last updated ${lastUpdated.toLocaleTimeString()}`
              : "Waiting for data..."}
          </p>
        </div>

        <div className="flex gap-3">
          <label className="flex flex-col text-sm">
            Host
            <input
              className="mt-1 rounded-md border border-zinc-300 bg-transparent px-3 py-1.5 dark:border-zinc-700"
              value={host}
              onChange={(e) => setHost(e.target.value)}
              placeholder="my-laptop.local"
            />
          </label>
          <label className="flex flex-col text-sm">
            Range
            <select
              className="mt-1 rounded-md border border-zinc-300 bg-transparent px-3 py-1.5 dark:border-zinc-700"
              value={range}
              onChange={(e) => setRange(e.target.value)}
            >
              {RANGES.map((r) => (
                <option key={r} value={r}>
                  Last {r}
                </option>
              ))}
            </select>
          </label>
        </div>
      </header>

      {!host && (
        <p className="rounded-md bg-zinc-100 p-3 text-sm dark:bg-zinc-900">
          Enter a host name to see its metrics.
        </p>
      )}

      {error && (
        <p className="rounded-md bg-red-100 p-3 text-sm text-red-800 dark:bg-red-950 dark:text-red-200">
          Could not load metrics: {error}
        </p>
      )}

      <div className="grid gap-6 md:grid-cols-2">
        <MetricChart title="CPU usage" points={cpu} color="#3b82f6" />
        <MetricChart title="Memory usage" points={mem} color="#10b981" />
      </div>
    </main>
  );
}
