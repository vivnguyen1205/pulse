"use client";

import { useState } from "react";
import {
  CartesianGrid,
  Line,
  LineChart,
  ReferenceArea,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { AlertEpisode } from "./types";

export type Point = { timestamp: number; value: number };

type Props = {
  title: string;
  points: Point[];
  color: string;
  alerts: AlertEpisode[];
  now: number;
};

// Episode times are five-minute bucket starts, so a closed episode covers
// through the end of its last anomalous bucket. Ongoing episodes run to now.
const BUCKET_SECONDS = 300;

function alertEnd(alert: AlertEpisode, now: number) {
  return alert.ended_at === null ? now : alert.ended_at + BUCKET_SECONDS;
}

// Server sends Unix seconds; JavaScript dates use milliseconds.
function formatTime(ts: number) {
  return new Date(ts * 1000).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
  });
}

export default function MetricChart({ title, points, color, alerts, now }: Props) {
  const [hoveredAlert, setHoveredAlert] = useState<AlertEpisode | null>(null);
  const latest = points.length > 0 ? points[points.length - 1].value : null;
  const chartEnd = Math.max(
    points[points.length - 1]?.timestamp ?? now,
    ...alerts.map((alert) => alertEnd(alert, now)),
  );

  return (
    <section className="rounded-xl border border-zinc-200 p-4 dark:border-zinc-800">
      <div className="mb-3 flex items-baseline justify-between">
        <h2 className="font-semibold">{title}</h2>
        <span className="text-2xl font-bold tabular-nums">
          {latest === null ? "–" : `${latest.toFixed(1)}%`}
        </span>
      </div>

      {points.length === 0 ? (
        <div className="flex h-64 items-center justify-center text-sm text-zinc-500">
          No data in this range
        </div>
      ) : (
        <div className="relative h-64">
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={points}>
              <CartesianGrid strokeDasharray="3 3" opacity={0.3} />
              <XAxis
                dataKey="timestamp"
                type="number"
                domain={["dataMin", chartEnd]}
                tickFormatter={formatTime}
                fontSize={12}
              />
              <YAxis domain={[0, 100]} unit="%" fontSize={12} width={45} />
              <Tooltip
                labelFormatter={(ts) => new Date(Number(ts) * 1000).toLocaleTimeString()}
                formatter={(v) => [`${Number(v).toFixed(1)}%`, title]}
              />
              {alerts
                .filter((alert) => alertEnd(alert, now) >= points[0].timestamp)
                .map((alert) => (
                  <ReferenceArea
                    key={alert.id}
                    x1={Math.max(alert.started_at, points[0].timestamp)}
                    x2={alertEnd(alert, now)}
                    fill="#ef4444"
                    fillOpacity={0.16}
                    onMouseEnter={() => setHoveredAlert(alert)}
                    onMouseLeave={() => setHoveredAlert(null)}
                  />
                ))}
              <Line
                type="monotone"
                dataKey="value"
                stroke={color}
                strokeWidth={2}
                dot={false}
                isAnimationActive={false}
              />
            </LineChart>
          </ResponsiveContainer>
          {hoveredAlert && (
            <div className="pointer-events-none absolute right-2 top-2 z-10 rounded-md border border-zinc-200 bg-white/95 p-3 text-xs shadow-md dark:border-zinc-700 dark:bg-zinc-900/95">
              <p className="font-semibold">{hoveredAlert.detector} alert</p>
              <p>Peak value: {hoveredAlert.peak_value.toFixed(2)}%</p>
              <p>Peak score: {hoveredAlert.peak_score.toFixed(2)}</p>
            </div>
          )}
        </div>
      )}
    </section>
  );
}