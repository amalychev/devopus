import * as React from "react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ws } from "@ws/api"
import type { Current } from "@ws/app"
import { FormDialog } from "@ws/dialogs"
import { PageHeader, Select, useAction, useLoad, when } from "@ws/ui"

export function Overview({ current, onDeleted }: { current: Current; onDeleted: () => void }) {
  const { workspace, info } = current
  const admin = current.can("workspace.manage")
  const [dialog, setDialog] = React.useState<"rename" | "transfer" | "delete">()
  return (
    <>
      <PageHeader title={workspace.name} description={`Admin: ${workspace.adminEmail} · since ${when(workspace.createdAt, false)}`}>
        {admin && (
          <>
            <Button variant="outline" onClick={() => setDialog("rename")}>Rename</Button>
            <Button variant="outline" onClick={() => setDialog("transfer")}>Hand over</Button>
            <Button variant="destructive" onClick={() => setDialog("delete")}>Delete</Button>
          </>
        )}
      </PageHeader>
      <section className="flex max-w-3xl flex-col gap-6">
        <div className="flex flex-col gap-2">
          <h2 className="text-sm font-medium">Your role: {workspace.role}</h2>
          <p className="text-sm text-muted-foreground">What you may do in the workspace, and in each project you have a role in.</p>
        </div>
        <Permissions label="Workspace" perms={info.permissions} />
        {info.projects.map((p) => (
          <Permissions key={p.project} label={p.project} perms={p.permissions} />
        ))}
      </section>
      <Rename current={current} open={dialog === "rename"} onClose={() => setDialog(undefined)} />
      <Transfer current={current} open={dialog === "transfer"} onClose={() => setDialog(undefined)} />
      <Delete current={current} open={dialog === "delete"} onClose={() => setDialog(undefined)} onDeleted={onDeleted} />
    </>
  )
}

function Permissions({ label, perms }: { label: string; perms: string[] }) {
  return (
    <div className="flex flex-col gap-2 rounded-xl border p-4">
      <span className="text-sm font-medium">{label}</span>
      <div className="flex flex-wrap gap-1.5">
        {perms.map((p) => (
          <Badge key={p} variant="secondary" className="font-mono">{p}</Badge>
        ))}
      </div>
    </div>
  )
}

function Rename({ current, open, onClose }: { current: Current; open: boolean; onClose: () => void }) {
  const [name, setName] = React.useState(current.workspace.name)
  const act = useAction()
  return (
    <FormDialog open={open} onClose={onClose} title="Rename the workspace" submit="Rename" disabled={!name.trim()} busy={act.busy} error={act.error}
      onSubmit={() => act.run(async () => {
        await ws.renameWorkspace({ workspaceId: current.workspace.id, name: name.trim() })
        onClose()
        current.reload()
      })}>
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="rename">Name</FieldLabel>
          <Input id="rename" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}

function Transfer({ current, open, onClose }: { current: Current; open: boolean; onClose: () => void }) {
  const members = useLoad(() => ws.listMembers({ workspaceId: current.workspace.id }), [current.workspace.id, open])
  const [email, setEmail] = React.useState("")
  const act = useAction()
  const others = (members.data?.members ?? []).filter((m) => m.role !== "admin")
  return (
    <FormDialog open={open} onClose={onClose} title="Hand the workspace over"
      description="The member becomes the admin; you stay as a teamlead. A workspace has one admin."
      submit="Hand over" danger disabled={!email} busy={act.busy} error={act.error}
      onSubmit={() => act.run(async () => {
        await ws.transferWorkspace({ workspaceId: current.workspace.id, email })
        onClose()
        current.reload()
      })}>
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="transfer">New admin</FieldLabel>
          <Select id="transfer" value={email} onChange={setEmail}
            options={[{ value: "", label: "Choose a member" }, ...others.map((m) => ({ value: m.email, label: `${m.email} (${m.role})` }))]} />
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}

function Delete({ current, open, onClose, onDeleted }: { current: Current; open: boolean; onClose: () => void; onDeleted: () => void }) {
  const [name, setName] = React.useState("")
  const act = useAction()
  const target = current.workspace.name
  return (
    <FormDialog open={open} onClose={onClose} title={`Delete ${target}`}
      description="Its projects, members, invites and secrets go too. It cannot be undone."
      submit="Delete" danger disabled={name !== target} busy={act.busy} error={act.error}
      onSubmit={() => act.run(async () => {
        await ws.deleteWorkspace({ workspaceId: current.workspace.id, confirmName: name })
        onClose()
        onDeleted()
      })}>
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="delete">Type the name to confirm</FieldLabel>
          <Input id="delete" value={name} onChange={(e) => setName(e.target.value)} />
          <FieldDescription>{target}</FieldDescription>
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}
