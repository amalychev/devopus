import { Code, ConnectError } from "@connectrpc/connect"
import { parse, stringify } from "yaml"

import type { ProjectConfig } from "@/gen/devopus/workspace/v1/workspace_pb"

// The project's settings in the YAML form of internal/spec, as plain objects: the page
// reads them and sends one application or service back.

export type AppSpec = {
  repo?: string
  path?: string
  branch?: string
  image?: string
  mount?: string
  routes?: string[]
  port?: number
  uses?: string[]
  env?: Record<string, string>
  command?: string
  hooks?: Record<string, string>
  [k: string]: unknown
}

export type ServiceSpec = {
  type: string
  version?: string
  image?: string
  apps?: string[]
  uses?: string[]
  port?: number
  ui_port?: number
  expose?: string
  env?: Record<string, string>
  command?: string
  replicas?: number
  schedule?: string
  [k: string]: unknown
}

export type ProjectSpec = {
  name: string
  domain: string
  apps?: Record<string, AppSpec>
  services?: Record<string, ServiceSpec>
  features?: Record<string, unknown>
  overrides?: { allowed?: string[] }
  data?: unknown
}

export function parseProject(c: ProjectConfig): ProjectSpec {
  return (parse(c.yaml) ?? {}) as ProjectSpec
}

// toYAML drops empty fields, as the server's YAML does.
export function toYAML(v: object): string {
  const clean = Object.fromEntries(
    Object.entries(v).filter(([, x]) => x !== undefined && x !== "" && x !== 0 && !(Array.isArray(x) && x.length === 0) &&
      !(x && typeof x === "object" && !Array.isArray(x) && Object.keys(x).length === 0))
  )
  return stringify(clean)
}

export function isConflict(err: unknown): boolean {
  return ConnectError.from(err).code === Code.Aborted
}

// env as lines KEY=value, for a textarea.
export function envText(env?: Record<string, string>): string {
  return Object.entries(env ?? {}).map(([k, v]) => `${k}=${v}`).join("\n")
}

export function parseEnv(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of text.split("\n")) {
    const t = line.trim()
    if (!t || t.startsWith("#")) continue
    const i = t.indexOf("=")
    if (i <= 0) throw new Error(`"${t}": KEY=value`)
    out[t.slice(0, i).trim()] = t.slice(i + 1)
  }
  return out
}

export function list(text: string): string[] {
  return text.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean)
}
