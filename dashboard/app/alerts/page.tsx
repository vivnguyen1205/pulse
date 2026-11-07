"use client";

import { useEffect, useState, type FormEvent } from "react";
import type { AlertEpisode } from "../types";

const API_URL = process.env.NEXT_PUBLIC_PULSE_API ?? "http://localhost:8080";
const REFRESH_MS = 15000;

function formatDate(timestamp: number) {
  return new Date(timestamp * 1000).toLocaleString();
}

function formatDuration(seconds: number) {
  const totalMinutes = Math.floor(seconds / 60);
  if (totalMinutes < 1) return `${seconds}s`;
  const minutes = totalMinutes % 60;
  const totalHours = Math.floor(totalMinutes / 60);
  if (totalHours < 1) return `${minutes}m`;
  const hours = totalHours % 24;
  const days = Math.floor(totalHours / 24);
  if (days > 0) return `${days}d ${hours}h ${minutes}m`;
  return `${hours}h ${minutes}m`;
}

export default function AlertsPage() {
  const [hostInput, setHostInput] = useState("");
  const [nameInput, setNameInput] = useState("");
  const [filters, setFilters] = useState({ host: "", name: "" });
  const [alerts, setAlerts] = useState<AlertEpisode[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(0);

  useEffect(() => {
    let cancelled = false;

    async function load() {
      try {
        const params = new URLSearchParams({ since: "24h", limit: "500" });
        if (filters.host) params.set("host", filters.host);
        if (filters.name) params.set("name", filters.name);
        const response = await fetch(`${API_URL}/alerts?${params}`);
        if (!response.ok) {
          throw new Error(`${response.status}: ${await response.text()}`);
        }
        const data: AlertEpisode[] = await response.json();
        if (cancelled) return;
        setAlerts(data);
        setError(null);
        setNow(Date.now());
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      }
    }

    load();
    const refresh = setInterval(load, REFRESH_MS);
    const clock = setInterval(() => setNow(Date.now()), 1000);
    return () => {
      cancelled = true;
      clearInterval(refresh);
      clearInterval(clock);
    };
  }, [filters]);

  function applyFilters(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setFilters({ host: hostInput.trim(), name: nameInput.trim() });
  }

  return (
    <main className="mx-auto w-full max-w-5xl flex-1 space-y-6 p-6">
      <header>
        <h1 className="text-3xl font-bold">Alert episodes</h1>
        <p className="mt-1 text-sm text-zinc-500">Recent episodes from the last 24 hours.</p>
      </header>

      <form onSubmit={applyFilters} className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col text-sm">
          Host
          <input
            className="mt-1 rounded-md border border-zinc-300 bg-transparent px-3 py-1.5 dark:border-zinc-700"
            value={hostInput}
            onChange={(event) => setHostInput(event.target.value)}
            placeholder="Any host"
          />
        </label>
        <label className="flex flex-col text-sm">
          Metric
          <input
            className="mt-1 rounded-md border border-zinc-300 bg-transparent px-3 py-1.5 dark:border-zinc-700"
            value={nameInput}
            onChange={(event) => setNameInput(event.target.value)}
            placeholder="Any metric"
          />
        </label>
        <button
          type="submit"
          className="rounded-md bg-zinc-900 px-4 py-2 text-sm text-white hover:bg-zinc-700 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
        >
          Filter
        </button>
      </form>

      {error && (
        <p className="rounded-md bg-red-100 p-3 text-sm text-red-800 dark:bg-red-950 dark:text-red-200">
          Could not load alerts: {error}
        </p>
      )}

      {alerts.length === 0 ? (
        <p className="rounded-xl border border-zinc-200 p-6 text-center text-sm text-zinc-500 dark:border-zinc-800">
          No recent alert episodes match these filters.
        </p>
      ) : (
        <div className="overflow-x-auto rounded-xl border border-zinc-200 dark:border-zinc-800">
          <table className="w-full min-w-[760px] text-left text-sm">
            <thead className="bg-zinc-100 text-xs uppercase text-zinc-600 dark:bg-zinc-900 dark:text-zinc-300">
              <tr>
                <th className="px-4 py-3">Host</th>
                <th className="px-4 py-3">Metric</th>
                <th className="px-4 py-3">Started</th>
                <th className="px-4 py-3">Ended</th>
                <th className="px-4 py-3">Duration</th>
                <th className="px-4 py-3">Peak score</th>
                <th className="px-4 py-3">Status</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-200 dark:divide-zinc-800">
              {alerts.map((alert) => {
                const ongoing = alert.ended_at === null;
                const end = alert.ended_at ?? Math.floor(now / 1000);
                return (
                  <tr key={alert.id}>
                    <td className="px-4 py-3 font-medium">{alert.host}</td>
                    <td className="px-4 py-3">{alert.name}</td>
                    <td className="px-4 py-3">{formatDate(alert.started_at)}</td>
                    <td className="px-4 py-3">
                      {alert.ended_at === null ? "—" : formatDate(alert.ended_at)}
                    </td>
                    <td className="px-4 py-3">{formatDuration(Math.max(0, end - alert.started_at))}</td>
                    <td className="px-4 py-3 tabular-nums">{alert.peak_score.toFixed(2)}</td>
                    <td className="px-4 py-3">
                      <span
                        className={
                          ongoing
                            ? "rounded-full bg-amber-100 px-2 py-1 text-xs text-amber-900 dark:bg-amber-950 dark:text-amber-200"
                            : "rounded-full bg-zinc-100 px-2 py-1 text-xs text-zinc-700 dark:bg-zinc-800 dark:text-zinc-200"
                        }
                      >
                        {ongoing ? "Ongoing" : "Closed"}
                      </span>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </main>
  );
}
