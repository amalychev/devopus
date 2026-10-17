import * as React from "react"
import { EllipsisIcon } from "lucide-react"

import { Button, buttonVariants } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty"
import { FieldGroup } from "@/components/ui/field"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { cn } from "@/lib/utils"
import { ws } from "@ws/api"
import type { Current } from "@ws/app"
import { FormDialog } from "@ws/dialogs"
import { AreaField, TextField } from "@ws/project/fields"
import { envText, list, parseEnv, toYAML, type AppSpec } from "@ws/project/model"
import type { Project } from "@ws/project/use-project"
import { Select, useAction } from "@ws/ui"

export function Apps({ current, project }: { current: Current; project: Project }) {
  const apps = project.spec.apps ?? {}
  const names = Object.keys(apps).sort()
  const services = project.spec.services ?? {}
  const can = current.can("apps.manage", project.name)
  const [edit, setEdit] = React.useState<{ name?: string }>()
  const attached = (app: string) => Object.keys(services).filter((s) => services[s].apps?.includes(app)).sort()
  return (
    <>
      <div className="mb-4 flex items-center justify-between gap-4">
        <p className="text-sm text-muted-foreground">Parts of the product, usually a repository each. What they run on is a service attached to them.</p>
        {can && <Button onClick={() => setEdit({})}>Add application</Button>}
      </div>
      {names.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>No applications yet</EmptyTitle>
            <EmptyDescription>An application is a repository the agents clone with their developers' git access.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Application</TableHead>
              <TableHead>Code</TableHead>
              <TableHead>Branch</TableHead>
              <TableHead>Routes</TableHead>
              <TableHead>Services</TableHead>
              <TableHead className="w-10" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {names.map((n) => {
              const a = apps[n]
              return (
                <TableRow key={n}>
                  <TableCell className="font-medium">{n}</TableCell>
                  <TableCell className="max-w-72 truncate font-mono text-xs">{a.repo || (a.path ? `./${a.path}` : a.image)}</TableCell>
                  <TableCell>{a.branch || "main"}</TableCell>
                  <TableCell className="font-mono text-xs">{(a.routes ?? []).join(" ") || "—"}</TableCell>
                  <TableCell>{attached(n).join(", ") || "—"}</TableCell>
                  <TableCell>
                    {can && (
                      <DropdownMenu>
                        <DropdownMenuTrigger aria-label={`Actions for ${n}`} className={cn(buttonVariants({ variant: "ghost", size: "icon-sm" }))}>
                          <EllipsisIcon />
                        </DropdownMenuTrigger>
                        <DropdownMenuContent>
                          <DropdownMenuItem onClick={() => setEdit({ name: n })}>Edit…</DropdownMenuItem>
                          <DropdownMenuItem variant="destructive"
                            onClick={() => project.change(`${n} removed`, (base) => ws.removeApp({ workspaceId: current.workspace.id, project: project.name, baseVersion: base, name: n }))}>
                            Remove
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    )}
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
      {edit && <AppForm key={edit.name ?? "new"} current={current} project={project} name={edit.name} onClose={() => setEdit(undefined)} />}
    </>
  )
}

function AppForm({ current, project, name, onClose }: { current: Current; project: Project; name?: string; onClose: () => void }) {
  const old: AppSpec = (name && project.spec.apps?.[name]) || {}
  const [n, setN] = React.useState(name ?? "")
  const [source, setSource] = React.useState(old.path && !old.repo ? "path" : "repo")
  const [code, setCode] = React.useState(old.repo ?? old.path ?? "")
  const [branch, setBranch] = React.useState(old.branch ?? "")
  const [routes, setRoutes] = React.useState((old.routes ?? []).join(" "))
  const [port, setPort] = React.useState(old.port ? String(old.port) : "")
  const [command, setCommand] = React.useState(old.command ?? "")
  const [env, setEnv] = React.useState(envText(old.env))
  const act = useAction()
  const create = !name
  return (
    <FormDialog open onClose={onClose} title={create ? "Add an application" : `Edit ${name}`} submit={create ? "Add" : "Save"}
      disabled={!n.trim() || !code.trim()} busy={act.busy} error={act.error}
      onSubmit={() => act.run(async () => {
        const app: AppSpec = {
          ...old,
          repo: source === "repo" ? code.trim() : undefined,
          path: source === "path" ? code.trim() : undefined,
          branch: branch.trim() || undefined,
          routes: list(routes),
          port: port ? Number(port) : undefined,
          command: command.trim() || undefined,
          env: parseEnv(env),
        }
        try {
          await ws.putApp({ workspaceId: current.workspace.id, project: project.name, baseVersion: project.config.version, name: n.trim(), appYaml: toYAML(app), create })
        } finally {
          project.reload()
        }
        onClose()
      })}>
      <FieldGroup>
        {create && <TextField id="app-name" label="Name" value={n} onChange={(v) => setN(v.toLowerCase())} hint="Lowercase letters, digits and dashes: frontend, api." />}
        <div className="grid grid-cols-[8rem_1fr] items-end gap-2">
          <Select value={source} onChange={setSource} options={[{ value: "repo", label: "Repository" }, { value: "path", label: "Folder" }]} />
          <TextField id="app-code" label={source === "repo" ? "Repository" : "Folder in the project's repository"} value={code} onChange={setCode} mono
            placeholder={source === "repo" ? "git@github.com:acme/api.git" : "api"} />
        </div>
        <TextField id="app-branch" label="Branch" value={branch} onChange={setBranch} placeholder="main" hint="Of the base environment; features get their own." />
        <TextField id="app-routes" label="Routes" value={routes} onChange={setRoutes} mono placeholder="/api" hint="Paths it serves, space-separated." />
        <TextField id="app-port" label="Port" type="number" value={port} onChange={setPort} placeholder="8080" />
        <TextField id="app-command" label="Command" value={command} onChange={setCommand} mono placeholder="npm run dev" />
        <AreaField id="app-env" label="Environment" value={env} onChange={setEnv} placeholder={"API_URL=${{ api.url }}\nDB_PASSWORD=${{ secrets.DB_PASSWORD }}"}
          hint="KEY=value per line; ${{ … }} refers to other applications, services and secrets." />
      </FieldGroup>
    </FormDialog>
  )
}
