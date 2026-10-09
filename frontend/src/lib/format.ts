/** Bytes, rendered the way a person would say them out loud. */
export function formatBytes(bytes: number): string {
  if (bytes < 1024) {
    return `${bytes} B`
  }

  const units = ["KB", "MB", "GB"]
  let value = bytes / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`
}

export function formatFileCount(count: number): string {
  return count === 1 ? "1 file" : `${count} files`
}

/** A deadline as a short, plain phrase such as "in 59 minutes". */
export function formatUntil(deadline: string): string {
  const remaining = new Date(deadline).getTime() - Date.now()
  if (Number.isNaN(remaining)) {
    return "at an unknown time"
  }
  if (remaining <= 0) {
    return "now"
  }

  const minutes = Math.ceil(remaining / 60_000)
  if (minutes < 60) {
    return `in ${plural(minutes, "minute")}`
  }

  const hours = Math.floor(minutes / 60)
  const restMinutes = minutes % 60
  if (hours < 24 && restMinutes === 0) {
    return `in ${plural(hours, "hour")}`
  }
  if (hours < 24) {
    return `in ${plural(hours, "hour")} ${plural(restMinutes, "minute")}`
  }

  const days = Math.floor(hours / 24)
  return `in ${plural(days, "day")}`
}

/** An RFC 3339 timestamp as a local date and time, for the claimed card. */
export function formatDateTime(stamp: string): string {
  const date = new Date(stamp)
  if (Number.isNaN(date.getTime())) {
    return stamp
  }
  return date.toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  })
}

function plural(count: number, unit: string): string {
  return `${count} ${unit}${count === 1 ? "" : "s"}`
}
