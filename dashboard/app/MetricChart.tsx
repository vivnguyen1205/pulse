"use client";

import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

export type Point = { timestamp: number; value: number };

type Props = {
  title: string;
  points: Point[];
  color: string;
};

// Server sends Unix seconds; JavaScript dates use milliseconds.
function formatTime(ts: number) {
  return new Date(ts * 1000).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
  });
}

export default function MetricChart({ title, points, color }: Props) {
  const latest = points.length > 0 ? points[points.length - 1].value : null;

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
        <div className="h-64">
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={points}>
              <CartesianGrid strokeDasharray="3 3" opacity={0.3} />
              <XAxis
                dataKey="timestamp"
                type="number"
                domain={["dataMin", "dataMax"]}
                tickFormatter={formatTime}
                fontSize={12}
              />
              <YAxis domain={[0, 100]} unit="%" fontSize={12} width={45} />
              <Tooltip
                labelFormatter={(ts) => new Date(Number(ts) * 1000).toLocaleTimeString()}
                formatter={(v) => [`${Number(v).toFixed(1)}%`, title]}
              />
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
        </div>
      )}
    </section>
  );
}