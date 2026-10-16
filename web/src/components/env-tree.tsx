import { PlusIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { PhaseDot } from "@/components/phase-dot"
import type { EnvironmentStatus } from "@/gen/devopus/agent/v2/agent_pb"
import { envTitle, isFeature, phaseKey, phaseLabel } from "@/lib/format"
import { cn } from "@/lib/utils"

export interface ProjectGroup {
  project: string
  base?: EnvironmentStatus
  features: EnvironmentStatus[]
}

export function groupByProject(
  envs: EnvironmentStatus[],
  projects: string[]
): ProjectGroup[] {
  const groups = new Map<string, ProjectGroup>()
  for (const p of projects) groups.set(p, { project: p, features: [] })
  for (const e of envs) {
    let g = groups.get(e.project)
    if (!g) {
      g = { project: e.project, features: [] }
      groups.set(e.project, g)
    }
    if (isFeature(e)) g.features.push(e)
    else g.base = e
  }
  for (const g of groups.values())
    g.features.sort((a, b) => envTitle(a).localeCompare(envTitle(b)))
  return [...groups.values()].sort((a, b) => a.project.localeCompare(b.project))
}

// The sidebar draws each project as git would: main is the trunk, features branch
// off it.
export function EnvTree({
  groups,
  selected,
  onSelect,
  onNewFeature,
}: {
  groups: ProjectGroup[]
  selected?: string
  onSelect: (id: string) => void
  onNewFeature: (project: string) => void
}) {
  return (
    <nav aria-label="Environments" className="flex flex-col gap-6">
      {groups.map((g) => (
        <section key={g.project} className="flex flex-col">
          <h2 className="px-3 pb-1.5 text-[0.8125rem] font-semibold tracking-tight text-sidebar-foreground/70">
            {g.project}
          </h2>
          <ul className="flex flex-col">
            {g.base && (
              <EnvRow
                env={g.base}
                selected={selected === g.base.id}
                onSelect={onSelect}
                trunk={g.features.length > 0}
              />
            )}
            {g.features.map((f, i) => (
              <EnvRow
                key={f.id}
                env={f}
                selected={selected === f.id}
                onSelect={onSelect}
                branch={i === g.features.length - 1 ? "last" : "mid"}
              />
            ))}
          </ul>
          {g.base && (
            <Button
              variant="ghost"
              size="sm"
              className="ml-6 w-fit text-sidebar-foreground/70"
              onClick={() => onNewFeature(g.project)}
            >
              <PlusIcon data-icon="inline-start" />
              New feature
            </Button>
          )}
        </section>
      ))}
    </nav>
  )
}

function EnvRow({
  env,
  selected,
  onSelect,
  trunk,
  branch,
}: {
  env: EnvironmentStatus
  selected: boolean
  onSelect: (id: string) => void
  trunk?: boolean
  branch?: "mid" | "last"
}) {
  const phase = phaseKey(env.phase)
  return (
    <li className="relative">
      {/* Trunk below main, and the elbow of each feature. */}
      {trunk && (
        <span
          aria-hidden
          className="pointer-events-none absolute top-1/2 bottom-0 left-[1.0625rem] z-10 border-l-2 border-sidebar-foreground/25"
        />
      )}
      {branch && (
        <span
          aria-hidden
          className="pointer-events-none absolute top-0 left-[1.0625rem] z-10 h-1/2 w-[1.125rem] rounded-bl-lg border-b-2 border-l-2 border-sidebar-foreground/25"
        />
      )}
      {branch === "mid" && (
        <span
          aria-hidden
          className="pointer-events-none absolute top-0 bottom-0 left-[1.0625rem] z-10 border-l-2 border-sidebar-foreground/25"
        />
      )}
      <button
        type="button"
        onClick={() => onSelect(env.id)}
        aria-current={selected ? "page" : undefined}
        className={cn(
          "relative flex w-full items-center gap-2.5 rounded-md py-1.5 pr-3 text-left text-sm outline-none focus-visible:ring-2 focus-visible:ring-sidebar-ring",
          branch ? "pl-9" : "pl-3",
          selected
            ? "bg-sidebar-accent font-medium text-sidebar-accent-foreground"
            : "text-sidebar-foreground hover:bg-sidebar-accent/60"
        )}
      >
        <PhaseDot phase={phase} className={cn(!branch && "ml-0.5")} />
        <span
          className={cn(
            "min-w-0 flex-1 truncate",
            phase === "sleeping" && "text-sidebar-foreground/60"
          )}
        >
          {envTitle(env)}
        </span>
        {phase !== "running" && (
          <span className="shrink-0 text-xs text-sidebar-foreground/55">
            {phaseLabel(env.phase)}
          </span>
        )}
      </button>
    </li>
  )
}
