import * as React from "react"
import { EllipsisIcon } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { buttonVariants } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { toast } from "@/components/ui/toast"
import type { Member } from "@/gen/devopus/workspace/v1/workspace_pb"
import { cn } from "@/lib/utils"
import { errorText, ws } from "@ws/api"
import type { Current } from "@ws/app"
import { FormDialog } from "@ws/dialogs"
import { ErrorNote, PageHeader, Select, useAction, useLoad, when } from "@ws/ui"

// The permissions given to a developer on top of the role (internal/authz.Grantable).
const grantable = ["project.configure", "apps.manage", "services.manage", "servers.manage", "secrets.manage"]

type Edit = { member: Member; kind: "grant" | "revoke" }

export function Members({ current }: { current: Current }) {
  const id = current.workspace.id
  const list = useLoad(() => ws.listMembers({ workspaceId: id }), [id])
  const [edit, setEdit] = React.useState<Edit>()
  const admin = current.can("workspace.manage")

  const act = async (what: string, fn: () => Promise<unknown>) => {
    try {
      await fn()
      toast.add({ title: what, type: "success" })
      list.reload()
      current.reload()
    } catch (err) {
      toast.add({ title: errorText(err), type: "error" })
    }
  }

  return (
    <>
      <PageHeader title="Members" description="Roles in the workspace and in its projects; permissions given on top of a developer's role." />
      <ErrorNote error={list.error} />
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Email</TableHead>
            <TableHead>Role</TableHead>
            <TableHead>Projects</TableHead>
            <TableHead>Joined</TableHead>
            <TableHead className="w-10" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {(list.data?.members ?? []).map((m) => {
            // What the caller may do to this member: the projects they manage where the
            // member is a developer, and removal from the workspace for the admin.
            const managed = m.projects.filter((p) => current.can("members.manage", p.project))
            const devs = managed.filter((p) => p.role === "developer")
            const withGrants = devs.filter((p) => p.grants.length > 0)
            const canRemove = admin && m.role !== "admin"
            const removable = managed.filter((p) => admin || p.role === "developer")
            const anything = devs.length > 0 || removable.length > 0 || canRemove
            return (
              <TableRow key={m.userId}>
                <TableCell className="font-medium">{m.email}</TableCell>
                <TableCell>
                  <Badge variant={m.role === "admin" ? "default" : "secondary"}>{m.role}</Badge>
                </TableCell>
                <TableCell>
                  <div className="flex flex-wrap gap-1.5">
                    {m.projects.map((p) => (
                      <Badge key={p.project} variant="outline">
                        {p.project} · {p.role}
                        {p.grants.length > 0 && ` + ${p.grants.join(", ")}`}
                      </Badge>
                    ))}
                  </div>
                </TableCell>
                <TableCell className="whitespace-nowrap">{when(m.joinedAt, false)}</TableCell>
                <TableCell>
                  {anything && (
                    <DropdownMenu>
                      <DropdownMenuTrigger aria-label={`Actions for ${m.email}`} className={cn(buttonVariants({ variant: "ghost", size: "icon-sm" }))}>
                        <EllipsisIcon />
                      </DropdownMenuTrigger>
                      <DropdownMenuContent className="min-w-56">
                        {devs.length > 0 && <DropdownMenuItem onClick={() => setEdit({ member: m, kind: "grant" })}>Give a permission…</DropdownMenuItem>}
                        {withGrants.length > 0 && <DropdownMenuItem onClick={() => setEdit({ member: m, kind: "revoke" })}>Take a permission back…</DropdownMenuItem>}
                        {removable.map((p) => (
                          <DropdownMenuItem key={p.project}
                            onClick={() => act(`${m.email} removed from ${p.project}`, () => ws.removeFromProject({ workspaceId: id, project: p.project, email: m.email }))}>
                            Remove from {p.project}
                          </DropdownMenuItem>
                        ))}
                        {canRemove && (
                          <>
                            <DropdownMenuSeparator />
                            <DropdownMenuItem variant="destructive"
                              onClick={() => act(`${m.email} removed; their access ended`, () => ws.removeMember({ workspaceId: id, email: m.email }))}>
                              Remove from the workspace
                            </DropdownMenuItem>
                          </>
                        )}
                      </DropdownMenuContent>
                    </DropdownMenu>
                  )}
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
      {edit && (
        <Grant key={edit.member.userId + edit.kind} current={current} edit={edit} onClose={() => setEdit(undefined)} onDone={() => (list.reload(), current.reload())} />
      )}
    </>
  )
}

function Grant({ current, edit, onClose, onDone }: { current: Current; edit: Edit; onClose: () => void; onDone: () => void }) {
  const { member, kind } = edit
  const projects = member.projects.filter((p) => p.role === "developer" && current.can("members.manage", p.project) && (kind === "grant" || p.grants.length > 0))
  const [project, setProject] = React.useState(projects[0]?.project ?? "")
  const had = projects.find((p) => p.project === project)?.grants ?? []
  const choices = kind === "grant" ? grantable.filter((g) => !had.includes(g) && current.can(g, project)) : had
  const [permission, setPermission] = React.useState("")
  const act = useAction()
  const give = kind === "grant"
  return (
    <FormDialog open onClose={onClose} title={give ? `Give ${member.email} a permission` : `Take a permission from ${member.email}`}
      description={give ? "On top of the developer's role, in one project." : undefined}
      submit={give ? "Give" : "Take back"} danger={!give} disabled={!project || !permission} busy={act.busy} error={act.error}
      onSubmit={() => act.run(async () => {
        const req = { workspaceId: current.workspace.id, project, email: member.email, permission }
        await (give ? ws.grantPermission(req) : ws.revokePermission(req))
        onClose()
        onDone()
      })}>
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="grant-project">Project</FieldLabel>
          <Select id="grant-project" value={project} onChange={(v) => (setProject(v), setPermission(""))}
            options={projects.map((p) => ({ value: p.project, label: p.project }))} />
        </Field>
        <Field>
          <FieldLabel htmlFor="grant-permission">Permission</FieldLabel>
          <Select id="grant-permission" value={permission} onChange={setPermission}
            options={[{ value: "", label: "Choose" }, ...choices.map((g) => ({ value: g, label: g }))]} />
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}
