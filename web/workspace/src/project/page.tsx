import { ArrowLeftIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { cn } from "@/lib/utils"
import type { Current } from "@ws/app"
import { Apps } from "@ws/project/apps"
import { Drift } from "@ws/project/drift"
import { History } from "@ws/project/history"
import { Secrets } from "@ws/project/secrets"
import { Services } from "@ws/project/services"
import { Settings } from "@ws/project/settings"
import { useProject } from "@ws/project/use-project"
import { ErrorNote } from "@ws/ui"

export const tabs = [
  { id: "apps", title: "Applications" },
  { id: "services", title: "Services" },
  { id: "secrets", title: "Secrets", needs: "secrets.read" },
  { id: "history", title: "History" },
  { id: "drift", title: "Drift" },
  { id: "settings", title: "Settings" },
] as const

export type Tab = (typeof tabs)[number]["id"]

// ProjectPage is 'devopus project …', 'app …', 'service …', 'secrets …' and 'drift' for
// one project, with the same calls and permissions.
export function ProjectPage({ current, name, tab, go }: { current: Current; name: string; tab: Tab; go: (path: string) => void }) {
  const { project, error } = useProject(current, name)
  const visible = tabs.filter((t) => !("needs" in t) || current.can(t.needs, name))
  return (
    <>
      <div className="mb-6 flex flex-col gap-3">
        <Button variant="ghost" size="sm" className="self-start" onClick={() => go("projects")}>
          <ArrowLeftIcon /> Projects
        </Button>
        <div className="flex flex-wrap items-end justify-between gap-2">
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">{name}</h1>
            {project && (
              <p className="text-sm text-muted-foreground">
                https://{project.spec.domain} · settings v{project.config.version} by {project.config.author}: {project.config.message}
              </p>
            )}
          </div>
        </div>
        <nav className="flex flex-wrap gap-1 border-b">
          {visible.map((t) => (
            <button key={t.id} onClick={() => go(`projects/${encodeURIComponent(name)}/${t.id}`)}
              className={cn("-mb-px border-b-2 border-transparent px-3 py-2 text-sm text-muted-foreground hover:text-foreground", tab === t.id && "border-foreground font-medium text-foreground")}>
              {t.title}
            </button>
          ))}
        </nav>
      </div>
      <ErrorNote error={error} />
      {!project ? (
        !error && <Skeleton className="h-40 w-full" />
      ) : tab === "services" ? (
        <Services current={current} project={project} />
      ) : tab === "secrets" ? (
        <Secrets current={current} project={project} />
      ) : tab === "history" ? (
        <History current={current} project={project} />
      ) : tab === "drift" ? (
        <Drift current={current} project={project} />
      ) : tab === "settings" ? (
        <Settings current={current} project={project} onDeleted={() => go("projects")} />
      ) : (
        <Apps current={current} project={project} />
      )}
    </>
  )
}
