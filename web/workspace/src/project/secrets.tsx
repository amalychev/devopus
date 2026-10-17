import * as React from "react"
import { Encrypter } from "age-encryption"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty"
import { FieldGroup } from "@/components/ui/field"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { toast } from "@/components/ui/toast"
import { errorText, ws } from "@ws/api"
import type { Current } from "@ws/app"
import { FormDialog } from "@ws/dialogs"
import { TextField } from "@ws/project/fields"
import type { Project } from "@ws/project/use-project"
import { ErrorNote, useAction, useLoad, when } from "@ws/ui"

// Secrets are end-to-end (ADR-0009): a value is encrypted here, in the browser, to the
// workspace's public key; the server stores the ciphertext and agents decrypt it.
export function Secrets({ current, project }: { current: Current; project: Project }) {
  const id = current.workspace.id
  const names = useLoad(() => ws.listSecrets({ workspaceId: id, project: project.name }), [id, project.name])
  const key = useLoad(() => ws.getSecretsKey({ workspaceId: id }), [id])
  const can = current.can("secrets.manage", project.name)
  const [setting, setSetting] = React.useState<string>()
  const remove = async (name: string) => {
    try {
      await ws.removeSecret({ workspaceId: id, project: project.name, name })
      toast.add({ title: `${name} removed`, type: "success" })
      names.reload()
    } catch (err) {
      toast.add({ title: errorText(err), type: "error" })
    }
  }
  const noKey = key.data && key.data.generation === 0
  return (
    <>
      <div className="mb-4 flex items-center justify-between gap-4">
        <p className="text-sm text-muted-foreground">Values settings refer to as {"${{ secrets.NAME }}"}. Encrypted in your browser; the server never sees them.</p>
        {can && !noKey && <Button onClick={() => setSetting("")}>Set a secret</Button>}
      </div>
      <ErrorNote error={names.error} />
      {noKey && (
        <Alert className="mb-4">
          <AlertDescription>
            The workspace has no secrets key yet. Its admin makes it on their machine with the first secret:{" "}
            <code className="font-mono">devopus secrets set {project.name} NAME</code>. After that, secrets can be set here too.
          </AlertDescription>
        </Alert>
      )}
      {names.data && names.data.secrets.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>No secrets</EmptyTitle>
            <EmptyDescription>Database passwords, API keys: agents put them into the environments.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Secret</TableHead>
              <TableHead>Set by</TableHead>
              <TableHead>When</TableHead>
              <TableHead className="w-40" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {(names.data?.secrets ?? []).map((s) => (
              <TableRow key={s.name}>
                <TableCell className="font-mono">{s.name}</TableCell>
                <TableCell>{s.updatedBy}</TableCell>
                <TableCell>{when(s.updatedAt)}</TableCell>
                <TableCell className="text-right">
                  {can && !noKey && (
                    <>
                      <Button variant="ghost" size="sm" onClick={() => setSetting(s.name)}>Change</Button>
                      <Button variant="ghost" size="sm" onClick={() => remove(s.name)}>Remove</Button>
                    </>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {setting !== undefined && (
        <SetSecret current={current} project={project} name={setting} onClose={() => setSetting(undefined)} onSet={names.reload} />
      )}
    </>
  )
}

function SetSecret({ current, project, name, onClose, onSet }: { current: Current; project: Project; name: string; onClose: () => void; onSet: () => void }) {
  const [n, setN] = React.useState(name)
  const [value, setValue] = React.useState("")
  const act = useAction()
  return (
    <FormDialog open onClose={onClose} title={name ? `Change ${name}` : "Set a secret"}
      description="The value is encrypted here with the workspace's key before it leaves the browser."
      submit="Set" disabled={!n.trim() || !value} busy={act.busy} error={act.error}
      onSubmit={() => act.run(async () => {
        // The key may be rotated meanwhile: encrypt to the current one, once more if refused.
        for (let attempt = 0; ; attempt++) {
          const k = await ws.getSecretsKey({ workspaceId: current.workspace.id })
          const e = new Encrypter()
          e.addRecipient(k.publicKey)
          const ciphertext = await e.encrypt(value)
          try {
            await ws.setSecret({ workspaceId: current.workspace.id, project: project.name, name: n.trim(), generation: k.generation, ciphertext })
            break
          } catch (err) {
            if (attempt > 0 || !errorText(err).includes("generation")) throw err
          }
        }
        setValue("")
        onClose()
        onSet()
      })}>
      <FieldGroup>
        {!name && <TextField id="secret-name" label="Name" value={n} onChange={setN} mono placeholder="DB_PASSWORD" />}
        <TextField id="secret-value" label="Value" type="password" value={value} onChange={setValue} mono />
      </FieldGroup>
    </FormDialog>
  )
}
