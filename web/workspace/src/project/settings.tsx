import * as React from "react"

import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldDescription, FieldGroup, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { ws } from "@ws/api"
import type { Current } from "@ws/app"
import { FormDialog } from "@ws/dialogs"
import { TextField } from "@ws/project/fields"
import { list } from "@ws/project/model"
import type { Project } from "@ws/project/use-project"
import { ErrorNote, useAction } from "@ws/ui"
import { stringify } from "yaml"

const kinds = [
  { id: "versions", label: "Versions of services", hint: "php 8.4 instead of 8.3" },
  { id: "env", label: "Any environment variable" },
  { id: "ports", label: "Ports" },
  { id: "*", label: "Everything devopus.yaml can say" },
]

export function Settings({ current, project, onDeleted }: { current: Current; project: Project; onDeleted: () => void }) {
  const can = current.can("project.configure", project.name)
  const allowed = project.spec.overrides?.allowed ?? []
  const [domain, setDomain] = React.useState(project.spec.domain)
  const [chosen, setChosen] = React.useState(allowed.filter((a) => !a.startsWith("env:")))
  const [vars, setVars] = React.useState(allowed.filter((a) => a.startsWith("env:")).map((a) => a.slice(4)).join(" "))
  const [deleting, setDeleting] = React.useState(false)
  const act = useAction()
  const toggle = (k: string, on: boolean) => setChosen((p) => (on ? [...p, k] : p.filter((x) => x !== k)))
  const save = (e: React.FormEvent) => {
    e.preventDefault()
    const next = [...chosen, ...list(vars).map((v) => `env:${v}`)]
    act.run(async () => {
      try {
        await ws.updateProjectSettings({
          workspaceId: current.workspace.id, project: project.name, baseVersion: project.config.version,
          domain: domain !== project.spec.domain ? domain.trim() : "",
          policyYaml: stringify({ overrides: { allowed: next } }),
        })
      } finally {
        project.reload()
      }
    })
  }
  return (
    <div className="flex max-w-xl flex-col gap-8">
      <form onSubmit={save}>
        <FieldGroup>
          <ErrorNote error={act.error} />
          <TextField id="domain" label="Domain" value={domain} onChange={setDomain} mono hint={`Environments live on https://${domain} and https://<feature>.${domain}.`} />
          <FieldSet>
            <FieldLegend variant="label">What developers may change for themselves</FieldLegend>
            <FieldDescription>In devopus.yaml in the project's folder on their machine; teamleads and the admin may change anything. Drift shows it.</FieldDescription>
            {kinds.map((k) => (
              <Field key={k.id} orientation="horizontal">
                <Checkbox id={`allow-${k.id}`} disabled={!can} checked={chosen.includes(k.id)} onCheckedChange={(on) => toggle(k.id, on === true)} />
                <FieldLabel htmlFor={`allow-${k.id}`} className="font-normal">{k.label}{k.hint && <span className="text-muted-foreground"> — {k.hint}</span>}</FieldLabel>
              </Field>
            ))}
            <Field>
              <FieldLabel htmlFor="allow-vars">Only these variables</FieldLabel>
              <Input id="allow-vars" disabled={!can} className="font-mono" placeholder="DEBUG XDEBUG_MODE" value={vars} onChange={(e) => setVars(e.target.value)} />
            </Field>
          </FieldSet>
          {can && (
            <Button type="submit" className="self-start" disabled={act.busy}>
              {act.busy && <Spinner />}
              Save
            </Button>
          )}
        </FieldGroup>
      </form>
      {current.can("members.manage", project.name) && (
        <div className="flex flex-col gap-2 rounded-xl border border-destructive/30 p-4">
          <span className="text-sm font-medium">Delete the project</span>
          <p className="text-sm text-muted-foreground">Its settings with every version, its roles and secrets go. Machines stop running it.</p>
          <Button variant="destructive" className="self-start" onClick={() => setDeleting(true)}>Delete {project.name}</Button>
        </div>
      )}
      <DeleteProject current={current} project={project} open={deleting} onClose={() => setDeleting(false)} onDeleted={onDeleted} />
    </div>
  )
}

function DeleteProject({ current, project, open, onClose, onDeleted }: { current: Current; project: Project; open: boolean; onClose: () => void; onDeleted: () => void }) {
  const [name, setName] = React.useState("")
  const act = useAction()
  return (
    <FormDialog open={open} onClose={onClose} title={`Delete ${project.name}`} description="It cannot be undone." submit="Delete" danger
      disabled={name !== project.name} busy={act.busy} error={act.error}
      onSubmit={() => act.run(async () => {
        await ws.deleteProject({ workspaceId: current.workspace.id, project: project.name, confirmName: name })
        onClose()
        current.reload()
        onDeleted()
      })}>
      <FieldGroup>
        <TextField id="delete-project" label="Type the name to confirm" value={name} onChange={setName} />
      </FieldGroup>
    </FormDialog>
  )
}
