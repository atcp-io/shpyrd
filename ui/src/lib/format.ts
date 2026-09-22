export function ago(iso: string | undefined): string {
  if (!iso) return "-";
  const ms = Date.now() - new Date(iso).getTime();
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

export function duration(from: string, to?: string): string {
  const ms =
    (to ? new Date(to).getTime() : Date.now()) - new Date(from).getTime();
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${s % 60}s`;
}

export function bytes(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(2)} GiB`;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(0)} KiB`;
  return `${n.toFixed(0)} B`;
}

/** Formats a metric value with its unit for axes and tooltips. */
export function metricValue(v: number, unit: string): string {
  if (!Number.isFinite(v)) return "-";
  switch (unit) {
    case "bytes":
      return bytes(v);
    case "bytes/s":
      return `${bytes(v)}/s`;
    case "ms":
      return v >= 1000 ? `${(v / 1000).toFixed(2)} s` : `${v.toFixed(0)} ms`;
    case "cores":
      return v < 1 ? `${(v * 1000).toFixed(0)} m` : `${v.toFixed(2)} cores`;
    case "rps":
      return `${v.toFixed(v < 10 ? 2 : 0)} rps`;
    case "count":
      return `${Math.round(v)}`;
    case "%":
      return `${v.toFixed(v < 10 ? 1 : 0)}%`;
    default:
      return v.toFixed(2);
  }
}

export function timeLabel(unixSeconds: number, range: string): string {
  const d = new Date(unixSeconds * 1000);
  if (range === "7d" || range === "24h") {
    return d.toLocaleString(undefined, {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  }
  return d.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function localTime(iso: string | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  return d.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}
