import * as React from "react"

import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"

// Confirm asks before something that cannot be undone. onConfirm may throw: the
// dialog stays open with the error, and retry (if given) offers a second action, such
// as deleting despite unsaved work.
export function Confirm({
  open,
  onOpenChange,
  title,
  description,
  action,
  onConfirm,
  retry,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description: React.ReactNode
  action: string
  onConfirm: () => Promise<void>
  retry?: {
    action: string
    onConfirm: () => Promise<void>
    when: (err: unknown) => string | undefined
  }
}) {
  const [busy, setBusy] = React.useState(false)
  const [problem, setProblem] = React.useState<string>()
  const [canRetry, setCanRetry] = React.useState(false)

  const change = (next: boolean) => {
    if (!next) {
      setProblem(undefined)
      setCanRetry(false)
    }
    onOpenChange(next)
  }

  const run = async (fn: () => Promise<void>, mayRetry: boolean) => {
    setBusy(true)
    try {
      await fn()
      change(false)
    } catch (err) {
      const why = mayRetry ? retry?.when(err) : undefined
      setProblem(why ?? (err instanceof Error ? err.message : String(err)))
      setCanRetry(!!why)
    } finally {
      setBusy(false)
    }
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent className="sm:max-w-md">
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        {problem && (
          <pre className="max-h-48 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap text-foreground">
            {problem}
          </pre>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={busy}>Cancel</AlertDialogCancel>
          {canRetry && retry ? (
            <Button
              variant="destructive"
              disabled={busy}
              onClick={() => run(retry.onConfirm, false)}
            >
              {busy && <Spinner data-icon="inline-start" />}
              {retry.action}
            </Button>
          ) : (
            <Button
              variant="destructive"
              disabled={busy}
              onClick={() => run(onConfirm, true)}
            >
              {busy && <Spinner data-icon="inline-start" />}
              {action}
            </Button>
          )}
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
