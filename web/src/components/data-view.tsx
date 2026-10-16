import * as React from "react"

import { Confirm } from "@/components/confirm"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { toast } from "@/components/ui/toast"
import type { EnvironmentStatus } from "@/gen/devopus/agent/v2/agent_pb"
import { agent, errorText } from "@/lib/api"
import { ago, date, isFeature } from "@/lib/format"

// DataView shows the databases of an environment: in a feature, copies of main's
// golden copy and whether main's migrations have moved past them.
export function DataView({ env }: { env: EnvironmentStatus }) {
  const [busy, setBusy] = React.useState<string>()
  const [confirm, setConfirm] = React.useState<string>()
  const feature = isFeature(env)

  const refresh = async (service: string) => {
    setBusy(service)
    try {
      await agent.resetData({ project: env.project, services: [service] })
      toast.add({
        title: `Refreshing the golden copy of ${service}`,
        description: "Features get it when their data is reset.",
        type: "success",
      })
    } catch (err) {
      toast.add({
        title: "Could not refresh the golden copy",
        description: errorText(err),
        type: "error",
      })
    } finally {
      setBusy(undefined)
    }
  }
  const reset = async (service: string) => {
    try {
      await agent.resetData({
        project: env.project,
        environment: env.id,
        services: [service],
      })
    } catch (err) {
      throw new Error(errorText(err), { cause: err })
    }
    toast.add({ title: `Copying ${service} from main again`, type: "success" })
  }

  if (env.data.length === 0) {
    return (
      <p className="py-6 text-sm text-muted-foreground">
        {feature
          ? "This feature has no database of its own: it uses main's, or the project has no Postgres or MySQL with a volume."
          : "No golden copies yet. They are taken when the first feature needs main's data."}
      </p>
    )
  }
  return (
    <div className="flex flex-col gap-3">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Database</TableHead>
            <TableHead>{feature ? "Copied" : "Golden copy taken"}</TableHead>
            <TableHead>Size</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {env.data.map((d) => (
            <TableRow key={d.service}>
              <TableCell>
                <span className="font-medium">{d.service}</span>{" "}
                <span className="text-muted-foreground">{d.engine}</span>
                {d.note && (
                  <div className="text-xs text-muted-foreground">{d.note}</div>
                )}
              </TableCell>
              <TableCell>
                {ago(date(d.copiedAt))}
                {d.behind && (
                  <div className="text-xs font-medium text-phase-creating">
                    main has newer migrations
                  </div>
                )}
              </TableCell>
              <TableCell className="tabular-nums">{d.size}</TableCell>
              <TableCell className="text-right">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={!!busy}
                  onClick={() =>
                    feature ? setConfirm(d.service) : refresh(d.service)
                  }
                >
                  {busy === d.service && <Spinner data-icon="inline-start" />}
                  {feature ? "Reset from main" : "Refresh"}
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <Confirm
        open={!!confirm}
        onOpenChange={(o) => !o && setConfirm(undefined)}
        title={`Reset ${confirm} from main?`}
        description="The feature's changes to this database are thrown away. It gets a fresh copy of main's data, and the feature's migrations run on it again."
        action="Reset"
        onConfirm={() => reset(confirm!)}
      />
    </div>
  )
}
