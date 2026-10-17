import { Badge } from "@/components/ui/badge"
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { sync } from "@ws/api"
import type { Current } from "@ws/app"
import type { Project } from "@ws/project/use-project"
import { ErrorNote, useLoad, when } from "@ws/ui"

// Drift is 'devopus drift --project': which machines run which version of the settings,
// with which personal overrides.
export function Drift({ current, project }: { current: Current; project: Project }) {
  const devices = useLoad(() => sync.listDevices({ workspaceId: current.workspace.id, project: project.name }), [current.workspace.id, project.name, project.config.version])
  const rows = (devices.data?.devices ?? []).flatMap((d) => d.environments.map((e) => ({ d, e })))
  return (
    <>
      <p className="mb-4 text-sm text-muted-foreground">
        Machines running {project.name}: yours, and everyone's in projects you lead. The current settings are v{project.config.version}.
      </p>
      <ErrorNote error={devices.error} />
      {devices.data && rows.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>No machine runs it yet</EmptyTitle>
            <EmptyDescription>Agents report here once they bring the project up.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Who</TableHead>
              <TableHead>Machine</TableHead>
              <TableHead>Environment</TableHead>
              <TableHead>Settings</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Personal overrides</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map(({ d, e }) => (
              <TableRow key={d.device!.id + d.userEmail + e.id}>
                <TableCell>{d.userEmail}</TableCell>
                <TableCell>
                  {d.device?.name}
                  {!d.online && <div className="text-xs text-muted-foreground">offline since {when(d.lastSeen)}</div>}
                </TableCell>
                <TableCell>{e.kind === "feature" ? e.name : "base"}</TableCell>
                <TableCell>
                  v{e.configVersion} {e.configVersion !== project.config.version && <Badge variant="destructive">behind</Badge>}
                </TableCell>
                <TableCell>
                  {e.phase}
                  {e.error && <div className="max-w-64 truncate text-xs text-destructive">{e.error}</div>}
                </TableCell>
                <TableCell className="text-xs">
                  {e.overrides.map((o) => <div key={o} className="font-mono">{o}</div>)}
                  {e.rejectedOverrides.map((o) => <div key={o} className="text-muted-foreground">refused: {o}</div>)}
                  {e.overrides.length + e.rejectedOverrides.length === 0 && "—"}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </>
  )
}
