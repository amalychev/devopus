import * as React from "react"

import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { ws } from "@ws/api"
import { ErrorNote, useAction } from "@ws/ui"

// FormDialog is a dialog with one form: the dialogs of this app.
export function FormDialog({
  open,
  onClose,
  title,
  description,
  submit,
  disabled,
  busy,
  error,
  danger,
  children,
  onSubmit,
}: {
  open: boolean
  onClose: () => void
  title: string
  description?: React.ReactNode
  submit: string
  disabled?: boolean
  busy?: boolean
  error?: string
  danger?: boolean
  children?: React.ReactNode
  onSubmit: () => void
}) {
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-md">
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            onSubmit()
          }}
        >
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            {description && <DialogDescription>{description}</DialogDescription>}
          </DialogHeader>
          <ErrorNote error={error} />
          {children}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" variant={danger ? "destructive" : "default"} disabled={disabled || busy}>
              {busy && <Spinner />}
              {submit}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

export function CreateWorkspace({ open, onClose, onCreated }: { open: boolean; onClose: () => void; onCreated: (id: string) => void }) {
  const [name, setName] = React.useState("")
  const act = useAction()
  return (
    <FormDialog
      open={open}
      onClose={() => (act.clear(), onClose())}
      title="New workspace"
      description="Your company's place in Devopus: its members and projects. You become its admin."
      submit="Create"
      disabled={!name.trim()}
      busy={act.busy}
      error={act.error}
      onSubmit={() =>
        act.run(async () => {
          const r = await ws.createWorkspace({ name: name.trim() })
          setName("")
          onClose()
          onCreated(r.workspace!.id)
        })
      }
    >
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="ws-name">Name</FieldLabel>
          <Input id="ws-name" autoFocus value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}

export function AcceptInvite({ open, onClose, onAccepted }: { open: boolean; onClose: () => void; onAccepted: (id: string) => void }) {
  const [code, setCode] = React.useState("")
  const act = useAction()
  return (
    <FormDialog
      open={open}
      onClose={() => (act.clear(), onClose())}
      title="Accept an invite"
      description="Paste the code from the invite email. It works for the email you are signed in with."
      submit="Join"
      disabled={!code.trim()}
      busy={act.busy}
      error={act.error}
      onSubmit={() =>
        act.run(async () => {
          const r = await ws.acceptInvite({ code: code.trim() })
          setCode("")
          onClose()
          onAccepted(r.workspace!.id)
        })
      }
    >
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="invite-code">Code</FieldLabel>
          <Input id="invite-code" autoFocus className="font-mono" placeholder="dvi_…" value={code} onChange={(e) => setCode(e.target.value)} />
        </Field>
      </FieldGroup>
    </FormDialog>
  )
}
