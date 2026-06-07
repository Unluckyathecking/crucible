"use client";

import { useMemo } from "react";
import type { DayBucket } from "@/lib/usage-format";

export function UsageChart({ buckets }: { buckets: DayBucket[] }) {
  const W = 600;
  const H = 120;
  const PAD_L = 48;
  const PAD_B = 20;
  const chartW = W - PAD_L;
  const chartH = H - PAD_B;

  const { maxUnits, barSlot, barW } = useMemo(() => {
    // Use reduce instead of spread to avoid call-stack limits on large arrays.
    const maxUnits = buckets.reduce((m, b) => Math.max(m, b.units), 0);
    const barSlot = chartW / Math.max(1, buckets.length);
    const barW = Math.max(1, barSlot - 1);
    return { maxUnits, barSlot, barW };
  }, [buckets]);

  if (buckets.length === 0) {
    return <p className="text-sm text-zinc-500">No data to visualize.</p>;
  }
  if (maxUnits === 0) {
    return <p className="text-sm text-zinc-500">No units recorded in this period.</p>;
  }

  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      className="w-full"
      role="img"
      aria-label="Units over time bar chart"
    >
      {/* Y axis */}
      <line x1={PAD_L} y1={0} x2={PAD_L} y2={chartH} stroke="#e4e4e7" strokeWidth="1" />
      {/* X axis */}
      <line x1={PAD_L} y1={chartH} x2={W} y2={chartH} stroke="#e4e4e7" strokeWidth="1" />

      {/* Y-axis labels */}
      <text aria-hidden="true" x={PAD_L - 4} y={10} textAnchor="end" fontSize="9" fill="#a1a1aa">
        {maxUnits.toLocaleString("en-US")}
      </text>
      <text aria-hidden="true" x={PAD_L - 4} y={chartH} textAnchor="end" fontSize="9" fill="#a1a1aa">
        0
      </text>

      {/* Bars */}
      {buckets.map((b, i) => {
        const barH = Math.max(1, (b.units / maxUnits) * chartH);
        const x = PAD_L + i * barSlot;
        const y = chartH - barH;
        return (
          <rect key={`${b.date}-${i}`} x={x + 0.5} y={y} width={barW} height={barH} fill="#18181b" rx="1">
            <title>{`${b.date}: ${b.units.toLocaleString("en-US")} units`}</title>
          </rect>
        );
      })}

      {/* X-axis labels: first and last date */}
      <text aria-hidden="true" x={PAD_L + 1} y={H - 4} fontSize="9" fill="#a1a1aa">
        {buckets[0].date}
      </text>
      {buckets.length > 1 && (
        <text aria-hidden="true" x={W - 1} y={H - 4} textAnchor="end" fontSize="9" fill="#a1a1aa">
          {buckets[buckets.length - 1].date}
        </text>
      )}
    </svg>
  );
}
