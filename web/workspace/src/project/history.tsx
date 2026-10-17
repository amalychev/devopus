import * as React from "react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { cn } from "@/lib/utils"
import { ws } from "@ws/api"
import type { Current } from "@ws/app"
import type { Project } from "@ws/project/use-project"
import { ErrorNote, useLoad, when } from "@ws/ui"

// History is 'devopus project config history' and 'show --version' with the rollback.
export function History({ current, project }: { current: Current; project: Project }) {
  const id = current.workspace.id
  const versions = useLoad(() => ws.listProjectVersions({ workspaceId: id, project: project.name, limit: 100 }), [id, project.name, project.config.version])
  const [shown, setShown] = React.useState(project.config.version)
  const config = useLoad(() => ws.getProjectConfig({ workspaceId: id, project: project.name, version: shown }), [id, project.name, shown, project.config.version])
  const can = current.can("project.configure", project.name)
  return (
    <div className="grid gap-6 lg:grid-cols-[minmax(0,26rem)_1fr]">
      <div>
        <ErrorNote error={versions.error} />
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Version</TableHead>
              <TableHead>What</TableHead>
              <TableHead>Who</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {(versions.data?.versions ?? []).map((v) => (
              <TableRow key={v.version} onClick={() => setShown(v.version)} className={cn("cursor-pointer", v.version === shown && "bg-muted")}>
                <TableCell>
                  v{v.version} {v.version === project.config.version && <Badge variant="secondary">current</Badge>}
                </TableCell>
                <TableCell className="max-w-56 truncate">{v.message}</TableCell>
                <TableCell className="text-muted-foreground">
                  {v.author}{" "}
                  {!v.signed && (
                    <Badge variant="outline" title="Made in the browser or by an older CLI: servers do not run it and agents ask before its new code. Review and sign it: devopus project sign">
                      unsigned
                    </Badge>
                  )}
                  <br />
                  <span className="text-xs">{when(v.createdAt)}</span>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <div className="flex min-w-0 flex-col gap-3">
        <div className="flex items-center justify-between gap-2">
          <span className="text-sm font-medium">Version {shown}</span>
          {can && shown !== project.config.version && (
            <Button variant="outline" size="sm"
              onClick={() => project.change(`${project.name} is back to version ${shown}`, () => ws.rollbackProject({ workspaceId: id, project: project.name, version: shown }))}>
              Make it current again
            </Button>
          )}
        </div>
        <pre className="overflow-auto rounded-xl border bg-muted/40 p-4 font-mono text-xs leading-relaxed">{config.data?.config?.yaml ?? ""}</pre>
      </div>
    </div>
  )
}
