import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { ws } from "@ws/api"
import type { Current } from "@ws/app"
import { ErrorNote, PageHeader, useLoad, when } from "@ws/ui"

export function Audit({ current }: { current: Current }) {
  const log = useLoad(() => ws.listAudit({ workspaceId: current.workspace.id, limit: 200 }), [current.workspace.id])
  return (
    <>
      <PageHeader title="Audit log" description="Who changed members, roles, permissions, projects and secrets, and when." />
      <ErrorNote error={log.error} />
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>When</TableHead>
            <TableHead>Who</TableHead>
            <TableHead>What</TableHead>
            <TableHead>Whom</TableHead>
            <TableHead>Detail</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {(log.data?.events ?? []).map((e, i) => (
            <TableRow key={i}>
              <TableCell className="whitespace-nowrap">{when(e.at)}</TableCell>
              <TableCell>{e.actor}</TableCell>
              <TableCell className="font-mono text-xs">{e.action}</TableCell>
              <TableCell>{e.target}</TableCell>
              <TableCell className="text-muted-foreground">{e.detail}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </>
  )
}
