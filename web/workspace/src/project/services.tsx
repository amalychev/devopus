import * as React from "react"
import { EllipsisIcon } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty"
import { Field, FieldDescription, FieldGroup, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import type { ServiceType } from "@/gen/devopus/workspace/v1/workspace_pb"
import { cn } from "@/lib/utils"
import { ws } from "@ws/api"
import type { Current } from "@ws/app"
import { FormDialog } from "@ws/dialogs"
import { AreaField, TextField } from "@ws/project/fields"
import { envText, parseEnv, toYAML, type ServiceSpec } from "@ws/project/model"
import type { Project } from "@ws/project/use-project"
import { Select, useAction, useLoad } from "@ws/ui"

export function Services({ current, project }: { current: Current; project: Project }) {
  const services = project.spec.services ?? {}
  const names = Object.keys(services).sort()
  const can = current.can("services.manage", project.name)
  const types = useLoad(() => ws.listServiceTypes({}), [])
  const kind = (t: string) => types.data?.types.find((x) => x.name === t || x.aliases.includes(t))?.kind ?? "container"
  const [edit, setEdit] = React.useState<{ name?: string }>()
  return (
    <>
      <div className="mb-4 flex items-center justify-between gap-4">
        <p className="text-sm text-muted-foreground">Services live in the project and are attached to its applications: runtimes, containers and processes.</p>
        {can && <Button onClick={() => setEdit({})}>Add service</Button>}
      </div>
      {names.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>No services yet</EmptyTitle>
            <EmptyDescription>php 8.3 for the backend, mysql 8.4, redis, a queue worker…</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Service</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Version</TableHead>
              <TableHead>Kind</TableHead>
              <TableHead>Applications</TableHead>
              <TableHead className="w-10" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {names.map((n) => {
              const s = services[n]
              return (
                <TableRow key={n}>
                  <TableCell className="font-medium">{n}</TableCell>
                  <TableCell>{s.type}</TableCell>
                  <TableCell className="font-mono text-xs">{s.version || s.image || "—"}</TableCell>
                  <TableCell><Badge variant="outline">{kind(s.type)}</Badge></TableCell>
                  <TableCell>{(s.apps ?? []).join(", ") || "—"}</TableCell>
                  <TableCell>
                    {can && (
                      <DropdownMenu>
                        <DropdownMenuTrigger aria-label={`Actions for ${n}`} className={cn(buttonVariants({ variant: "ghost", size: "icon-sm" }))}>
                          <EllipsisIcon />
                        </DropdownMenuTrigger>
                        <DropdownMenuContent>
                          <DropdownMenuItem onClick={() => setEdit({ name: n })}>Edit…</DropdownMenuItem>
                          <DropdownMenuItem variant="destructive"
                            onClick={() => project.change(`${n} removed`, (base) => ws.removeService({ workspaceId: current.workspace.id, project: project.name, baseVersion: base, name: n }))}>
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
      {edit && types.data && (
        <ServiceForm key={edit.name ?? "new"} current={current} project={project} name={edit.name} types={types.data.types} onClose={() => setEdit(undefined)} />
      )}
    </>
  )
}

function ServiceForm({ current, project, name, types, onClose }: { current: Current; project: Project; name?: string; types: ServiceType[]; onClose: () => void }) {
  const old: ServiceSpec = (name && project.spec.services?.[name]) || { type: "" }
  const create = !name
  const [type, setType] = React.useState(old.type)
  const [n, setN] = React.useState(name ?? "")
  const [version, setVersion] = React.useState(old.version ?? "")
  const [image, setImage] = React.useState(old.image ?? "")
  const [apps, setApps] = React.useState<string[]>(old.apps ?? [])
  const [command, setCommand] = React.useState(old.command ?? "")
  const [schedule, setSchedule] = React.useState(old.schedule ?? "")
  const [env, setEnv] = React.useState(envText(old.env))
  const act = useAction()
  const t = types.find((x) => x.name === type)
  const process = t?.kind === "process"
  const all = Object.keys(project.spec.apps ?? {}).sort()
  const toggle = (a: string, on: boolean) => setApps((p) => (on ? [...p, a].sort() : p.filter((x) => x !== a)))
  return (
    <FormDialog open onClose={onClose} title={create ? "Add a service" : `Edit ${name}`} submit={create ? "Add" : "Save"}
      disabled={!type || !(n || type)} busy={act.busy} error={act.error}
      onSubmit={() => act.run(async () => {
        const svc: ServiceSpec = {
          ...old,
          type,
          version: version.trim() || undefined,
          image: image.trim() || undefined,
          apps,
          command: command.trim() || undefined,
          schedule: schedule.trim() || undefined,
          env: parseEnv(env),
        }
        try {
          await ws.putService({ workspaceId: current.workspace.id, project: project.name, baseVersion: project.config.version, name: (n || type).trim(), serviceYaml: toYAML(svc), create })
        } finally {
          project.reload()
        }
        onClose()
      })}>
      <FieldGroup>
        {create && (
          <>
            <Field>
              <FieldLabel htmlFor="svc-type">Type</FieldLabel>
              <Select id="svc-type" value={type} onChange={setType}
                options={[{ value: "", label: "Choose a type" }, ...types.map((x) => ({ value: x.name, label: `${x.name} — ${x.kind}` }))]} />
            </Field>
            <TextField id="svc-name" label="Name" value={n} onChange={(v) => setN(v.toLowerCase())} placeholder={type || "db"} hint="Default: the type." />
          </>
        )}
        {!process && (
          <TextField id="svc-version" label="Version" value={version} onChange={setVersion} mono placeholder={t?.kind === "runtime" ? "8.3" : "17"}
            hint={t?.repo ? `The tag of ${t.repo}: everyone runs the same one.` : "A custom service names its image instead."} />
        )}
        {type === "custom" && <TextField id="svc-image" label="Image" value={image} onChange={setImage} mono placeholder="ghcr.io/acme/tool:1.2" />}
        {process && <TextField id="svc-command" label="Command" value={command} onChange={setCommand} mono placeholder="php artisan queue:work" />}
        {type === "cronjob" && <TextField id="svc-schedule" label="Schedule" value={schedule} onChange={setSchedule} mono placeholder="*/5 * * * *" />}
        <FieldSet>
          <FieldLegend variant="label">Attached to</FieldLegend>
          {all.length === 0 ? (
            <FieldDescription>No applications yet.</FieldDescription>
          ) : (
            all.map((a) => (
              <Field key={a} orientation="horizontal">
                <Checkbox id={`svc-app-${a}`} checked={apps.includes(a)} onCheckedChange={(on) => toggle(a, on === true)} />
                <FieldLabel htmlFor={`svc-app-${a}`} className="font-normal">{a}</FieldLabel>
              </Field>
            ))
          )}
        </FieldSet>
        <AreaField id="svc-env" label="Environment" value={env} onChange={setEnv} placeholder={"MYSQL_DATABASE=shop\nMYSQL_ROOT_PASSWORD=${{ secrets.DB_PASSWORD }}"} />
      </FieldGroup>
    </FormDialog>
  )
}
