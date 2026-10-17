import * as React from "react"

import { Button } from "@/components/ui/button"
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { ws } from "@ws/api"
import type { Current } from "@ws/app"
import { FormDialog } from "@ws/dialogs"
import { ErrorNote, PageHeader, useAction, useLoad, when } from "@ws/ui"

// Projects for now are names that roles hang on; their applications, services and
// versions come to the web with stage 4 (the CLI has them: devopus project …).
export function Projects({ current }: { current: Current }) {
  const list = useLoad(() => ws.listProjects({ workspaceId: current.workspace.id }), [current.workspace.id])
  const [creating, setCreating] = React.useState(false)
  const projects = list.data?.projects ?? []
  return (
    <>
      <PageHeader title="Projects" description="Products of the workspace. Settings, applications and services: devopus project show <name>.">
        {current.can("project.create") && <Button onClick={() => setCreating(true)}>New project</Button>}
      </PageHeader>
      <ErrorNote error={list.error} />
      {list.data && projects.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>No projects you work in</EmptyTitle>
            <EmptyDescription>A teamlead or the admin creates them and gives you a role.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Project</TableHead>
              <TableHead>Your role</TableHead>
              <TableHead>Settings version</TableHead>
              <TableHead>Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {projects.map((p) => (
              <TableRow key={p.id}>
                <TableCell className="font-medium">{p.name}</TableCell>
                <TableCell>{p.role || "admin"}</TableCell>
                <TableCell>v{p.version}</TableCell>
                <TableCell>{when(p.createdAt, false)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      <NewProject current={current} open={creating} onClose={() => setCreating(false)} onCreated={list.reload} />
    </>
  )
}

function NewProject({ current, open, onClose, onCreated }: { current: Current; open: boolean; onClose: () => void; onCreated: () => void }) {
  const [name, setName] = React.useState("")
  const act = useAction()
  return (
    <FormDialog open={open} onClose={onClose} title="New project" submit="Create" disabled={!name.trim()} busy={act.busy} error={act.error}
      onSubmit={() => act.run(async () => {
        await ws.createProject({ workspaceId: current.workspace.id, name: name.trim() })
        setName("")
        onClose()
        onCreated()
        current.reload()
      })}>
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="project">Name</FieldLabel>
          <Input id="project" autoFocus value={name} onChange={(e) => setName(e.target.value.toLowerCase())} />
          <FieldDescription>Lowercase letters, digits and dashes: it becomes a host, shop.localhost. A teamlead who creates a project leads it.</FieldDescription>
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}
