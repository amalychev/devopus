import * as React from "react"
import { CopyIcon } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty"
import { Field, FieldDescription, FieldGroup, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { toast } from "@/components/ui/toast"
import type { Invite } from "@/gen/devopus/workspace/v1/workspace_pb"
import { errorText, ws } from "@ws/api"
import type { Current } from "@ws/app"
import { FormDialog } from "@ws/dialogs"
import { ErrorNote, PageHeader, Select, useAction, useLoad, when } from "@ws/ui"

export function Invites({ current }: { current: Current }) {
  const id = current.workspace.id
  const list = useLoad(() => ws.listInvites({ workspaceId: id }), [id])
  const [inviting, setInviting] = React.useState(false)
  const [sent, setSent] = React.useState<{ invite: Invite; code: string }>()
  const invites = list.data?.invites ?? []

  const revoke = async (inv: Invite) => {
    try {
      await ws.revokeInvite({ workspaceId: id, inviteId: inv.id })
      toast.add({ title: `Invite of ${inv.email} revoked`, type: "success" })
      list.reload()
    } catch (err) {
      toast.add({ title: errorText(err), type: "error" })
    }
  }

  return (
    <>
      <PageHeader title="Invites" description="Open invites. The admin invites teamleads and developers; a teamlead, developers into the projects they lead.">
        <Button onClick={() => setInviting(true)}>Invite</Button>
      </PageHeader>
      <ErrorNote error={list.error} />
      {list.data && invites.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>No open invites</EmptyTitle>
            <EmptyDescription>An invite goes to an email with a role and works for 7 days.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Email</TableHead>
              <TableHead>Role</TableHead>
              <TableHead>Projects</TableHead>
              <TableHead>Invited by</TableHead>
              <TableHead>Until</TableHead>
              <TableHead className="w-24" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {invites.map((inv) => (
              <TableRow key={inv.id}>
                <TableCell className="font-medium">{inv.email}</TableCell>
                <TableCell><Badge variant="secondary">{inv.role}</Badge></TableCell>
                <TableCell>{inv.projects.join(", ") || "—"}</TableCell>
                <TableCell>{inv.invitedBy}</TableCell>
                <TableCell className="whitespace-nowrap">{when(inv.expiresAt, false)}</TableCell>
                <TableCell>
                  <Button variant="ghost" size="sm" onClick={() => revoke(inv)}>Revoke</Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      <NewInvite current={current} open={inviting} onClose={() => setInviting(false)}
        onSent={(invite, code) => (setSent({ invite, code }), list.reload())} />
      <Sent sent={sent} onClose={() => setSent(undefined)} />
    </>
  )
}

function NewInvite({ current, open, onClose, onSent }: { current: Current; open: boolean; onClose: () => void; onSent: (inv: Invite, code: string) => void }) {
  const admin = current.can("workspace.manage")
  const projects = useLoad(() => ws.listProjects({ workspaceId: current.workspace.id }), [current.workspace.id, open])
  // A teamlead invites into the projects they lead.
  const into = (projects.data?.projects ?? []).filter((p) => admin || current.can("members.invite", p.name))
  const [email, setEmail] = React.useState("")
  const [role, setRole] = React.useState("developer")
  const [chosen, setChosen] = React.useState<string[]>([])
  const act = useAction()
  const toggle = (name: string, on: boolean) => setChosen((c) => (on ? [...c, name] : c.filter((x) => x !== name)))
  return (
    <FormDialog open={open} onClose={() => (act.clear(), onClose())} title="Invite to the workspace"
      description="The invite is emailed; they sign in with that email and accept it."
      submit="Send invite" disabled={!email.trim() || (role === "developer" && chosen.length === 0)} busy={act.busy} error={act.error}
      onSubmit={() => act.run(async () => {
        const r = await ws.createInvite({ workspaceId: current.workspace.id, email: email.trim(), role, projects: chosen })
        setEmail("")
        setChosen([])
        onClose()
        onSent(r.invite!, r.code)
      })}>
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="invite-email">Email</FieldLabel>
          <Input id="invite-email" type="email" autoFocus value={email} onChange={(e) => setEmail(e.target.value)} />
        </Field>
        <Field>
          <FieldLabel htmlFor="invite-role">Role</FieldLabel>
          <Select id="invite-role" value={role} onChange={setRole}
            options={[{ value: "developer", label: "Developer" }, ...(admin ? [{ value: "teamlead", label: "Teamlead" }] : [])]} />
          <FieldDescription>
            {role === "teamlead" ? "Creates and sets up projects, invites developers into the ones they lead." : "Works with the environments of the projects below."}
          </FieldDescription>
        </Field>
        <FieldSet>
          <FieldLegend variant="label">Projects</FieldLegend>
          {into.length === 0 ? (
            <FieldDescription>No projects to invite into yet: create one first.</FieldDescription>
          ) : (
            into.map((p) => (
              <Field key={p.id} orientation="horizontal">
                <Checkbox id={`p-${p.id}`} checked={chosen.includes(p.name)} onCheckedChange={(on) => toggle(p.name, on === true)} />
                <FieldLabel htmlFor={`p-${p.id}`} className="font-normal">{p.name}</FieldLabel>
              </Field>
            ))
          )}
        </FieldSet>
      </FieldGroup>
    </FormDialog>
  )
}

// Sent shows the code once: the email carries it, and it can be passed on by hand.
function Sent({ sent, onClose }: { sent?: { invite: Invite; code: string }; onClose: () => void }) {
  const command = sent ? `devopus invite accept ${sent.code}` : ""
  return (
    <Dialog open={!!sent} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Invite sent to {sent?.invite.email}</DialogTitle>
          <DialogDescription>
            They sign in with that email and accept it here, or in the terminal. The code is shown only now.
          </DialogDescription>
        </DialogHeader>
        <div className="flex items-center gap-2 rounded-lg border bg-muted/40 p-3 font-mono text-xs break-all">
          <span className="flex-1">{command}</span>
          <Button variant="ghost" size="icon-sm" aria-label="Copy"
            onClick={() => navigator.clipboard?.writeText(command).then(() => toast.add({ title: "Copied", type: "success" }))}>
            <CopyIcon />
          </Button>
        </div>
        <DialogFooter>
          <Button onClick={onClose}>Done</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
