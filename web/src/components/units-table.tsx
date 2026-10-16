import * as React from "react"

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Badge } from "@/components/ui/badge"
import {
  Phase,
  type EnvironmentStatus,
  type UnitResources,
} from "@/gen/devopus/agent/v2/agent_pb"
import { agent } from "@/lib/api"
import { bytes } from "@/lib/format"
import { cn } from "@/lib/utils"

const RESOURCES_EVERY = 5000

function useResources(envId: string, active: boolean) {
  const [units, setUnits] = React.useState<Map<string, UnitResources>>(
    new Map()
  )
  React.useEffect(() => {
    if (!active) return
    let stopped = false
    let timer: number | undefined
    const load = async () => {
      try {
        const res = await agent.resources({ environmentId: envId })
        if (!stopped) setUnits(new Map(res.units.map((u) => [u.name, u])))
      } catch {
        // The table still shows states; usage comes back with the next sample.
      }
      if (!stopped) timer = window.setTimeout(load, RESOURCES_EVERY)
    }
    load()
    return () => {
      stopped = true
      window.clearTimeout(timer)
    }
  }, [envId, active])
  return active ? units : new Map<string, UnitResources>()
}

const stateClass: Record<string, string> = {
  running: "text-phase-running",
  starting: "text-phase-creating",
  unhealthy: "text-phase-error",
  exited: "text-phase-error",
}

export function UnitsTable({
  env,
  onLogs,
}: {
  env: EnvironmentStatus
  onLogs: (unit: string) => void
}) {
  const running = env.units.some(
    (u) => u.state === "running" || u.state === "starting"
  )
  const usage = useResources(env.id, running)
  if (env.units.length === 0) {
    return (
      <p className="py-6 text-sm text-muted-foreground">
        Nothing runs here yet. Applications and services appear once the agent starts
        them.
      </p>
    )
  }
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>State</TableHead>
          <TableHead>Routes</TableHead>
          <TableHead className="text-right">CPU</TableHead>
          <TableHead className="text-right">Memory</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {env.units.map((u) => {
          const r = usage.get(u.name)
          // A sleeping feature stops its containers on purpose: not a failure.
          const asleep = env.phase === Phase.SLEEPING && u.state === "exited"
          return (
            <TableRow key={u.name}>
              <TableCell>
                <div className="flex items-center gap-2">
                  <button
                    type="button"
                    className="font-medium underline-offset-4 hover:underline focus-visible:underline focus-visible:outline-none"
                    onClick={() => onLogs(u.name)}
                    title={`Show the output of ${u.name}`}
                  >
                    {u.name}
                  </button>
                  <Badge variant="outline" className="text-muted-foreground">
                    {u.role}
                  </Badge>
                </div>
                {u.address && (
                  <div className="font-mono text-xs text-muted-foreground">
                    {u.address}
                  </div>
                )}
              </TableCell>
              <TableCell>
                <span
                  className={cn(
                    "font-medium",
                    asleep ? "text-phase-sleeping" : stateClass[u.state]
                  )}
                >
                  {asleep ? "asleep" : u.state}
                </span>
                {u.error && !asleep && (
                  <div
                    className="max-w-80 truncate font-mono text-xs text-muted-foreground"
                    title={u.error}
                  >
                    {u.error}
                  </div>
                )}
              </TableCell>
              <TableCell className="font-mono text-xs">
                {u.routes.join("  ")}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {r ? `${r.cpuPercent.toFixed(r.cpuPercent < 10 ? 1 : 0)}%` : ""}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {r ? bytes(r.memoryBytes) : ""}
              </TableCell>
            </TableRow>
          )
        })}
      </TableBody>
    </Table>
  )
}
