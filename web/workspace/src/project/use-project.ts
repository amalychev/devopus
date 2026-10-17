import * as React from "react"

import { toast } from "@/components/ui/toast"
import type { ProjectConfig } from "@/gen/devopus/workspace/v1/workspace_pb"
import { errorText, ws } from "@ws/api"
import type { Current } from "@ws/app"
import { isConflict, parseProject, type ProjectSpec } from "@ws/project/model"

export type Project = {
  name: string
  config: ProjectConfig
  spec: ProjectSpec
  reload: () => void
  // change runs an edit made against the version on screen; if someone changed the
  // project meanwhile, the page shows the new version and asks to look again.
  change: (what: string, fn: (base: number) => Promise<unknown>) => Promise<boolean>
}

export function useProject(current: Current, name: string) {
  const [config, setConfig] = React.useState<ProjectConfig>()
  const [error, setError] = React.useState<string>()
  const [tick, setTick] = React.useState(0)
  React.useEffect(() => {
    let stale = false
    ws.getProjectConfig({ workspaceId: current.workspace.id, project: name })
      .then((r) => !stale && (setConfig(r.config), setError(undefined)))
      .catch((err) => !stale && setError(errorText(err)))
    return () => {
      stale = true
    }
  }, [current.workspace.id, name, tick])

  const reload = React.useCallback(() => setTick((t) => t + 1), [])
  const project: Project | undefined = config && {
    name,
    config,
    spec: parseProject(config),
    reload,
    change: async (what, fn) => {
      try {
        await fn(config.version)
        toast.add({ title: what, type: "success" })
        reload()
        return true
      } catch (err) {
        if (isConflict(err)) {
          toast.add({ title: "Someone changed the project meanwhile: here is the new version, look again.", type: "error" })
          reload()
        } else {
          toast.add({ title: errorText(err), type: "error" })
        }
        return false
      }
    },
  }
  return { project, error }
}
