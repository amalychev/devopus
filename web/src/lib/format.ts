import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt"

import {
  EnvironmentKind,
  Phase,
  type EnvironmentStatus,
} from "@/gen/devopus/agent/v2/agent_pb"

export type PhaseKey = "running" | "sleeping" | "creating" | "error" | "stopped"

export function phaseKey(p: Phase): PhaseKey {
  switch (p) {
    case Phase.RUNNING:
      return "running"
    case Phase.SLEEPING:
      return "sleeping"
    case Phase.ERROR:
      return "error"
    case Phase.STOPPED:
      return "stopped"
    default:
      return "creating"
  }
}

export function phaseLabel(p: Phase): string {
  switch (p) {
    case Phase.RUNNING:
      return "Running"
    case Phase.SLEEPING:
      return "Asleep"
    case Phase.ERROR:
      return "Failed"
    case Phase.STOPPED:
      return "Stopped"
    case Phase.DELETING:
      return "Deleting"
    default:
      return "Starting"
  }
}

export function isFeature(e: EnvironmentStatus): boolean {
  return e.kind === EnvironmentKind.FEATURE
}

export function envTitle(e: EnvironmentStatus): string {
  return isFeature(e) ? e.name || e.slug : "main"
}

export function date(t: Timestamp | undefined): Date | undefined {
  return t ? timestampDate(t) : undefined
}

const units: [Intl.RelativeTimeFormatUnit, number][] = [
  ["day", 86400],
  ["hour", 3600],
  ["minute", 60],
]
const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" })

export function ago(d: Date | undefined, now = Date.now()): string {
  if (!d) return ""
  const s = Math.round((d.getTime() - now) / 1000)
  for (const [unit, size] of units) {
    if (Math.abs(s) >= size) return rtf.format(Math.round(s / size), unit)
  }
  return "just now"
}

export function duration(from: Date | undefined, to: Date | undefined): string {
  if (!from || !to) return ""
  const ms = to.getTime() - from.getTime()
  if (ms < 1000) return `${ms} ms`
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`
  return `${Math.floor(ms / 60000)} min ${Math.round((ms % 60000) / 1000)} s`
}

export function bytes(n: bigint | number): string {
  let v = Number(n)
  const u = ["B", "KB", "MB", "GB", "TB"]
  let i = 0
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${u[i]}`
}
