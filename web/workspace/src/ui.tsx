import * as React from "react"
import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { errorText } from "@ws/api"

// useLoad runs a call when its inputs change and keeps the result, the error and a way
// to run it again.
export function useLoad<T>(fn: () => Promise<T>, deps: React.DependencyList) {
  const [data, setData] = React.useState<T>()
  const [error, setError] = React.useState<string>()
  const [tick, setTick] = React.useState(0)
  React.useEffect(() => {
    let stale = false
    fn()
      .then((d) => !stale && (setData(d), setError(undefined)))
      .catch((err) => !stale && setError(errorText(err)))
    return () => {
      stale = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick])
  return { data, error, reload: () => setTick((t) => t + 1) }
}

// useAction runs a change: busy while it runs, its error to show.
export function useAction() {
  const [busy, setBusy] = React.useState(false)
  const [error, setError] = React.useState<string>()
  const run = async (fn: () => Promise<unknown>): Promise<boolean> => {
    setBusy(true)
    setError(undefined)
    try {
      await fn()
      return true
    } catch (err) {
      setError(errorText(err))
      return false
    } finally {
      setBusy(false)
    }
  }
  return { busy, error, run, clear: () => setError(undefined) }
}

export function ErrorNote({ error }: { error?: string }) {
  if (!error) return null
  return (
    <Alert variant="destructive">
      <AlertDescription>{error}</AlertDescription>
    </Alert>
  )
}

export function PageHeader({ title, description, children }: { title: string; description?: string; children?: React.ReactNode }) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
        {description && <p className="text-sm text-muted-foreground">{description}</p>}
      </div>
      {children && <div className="flex gap-2">{children}</div>}
    </div>
  )
}

export function when(ts?: Timestamp, withTime = true): string {
  if (!ts) return "—"
  return timestampDate(ts).toLocaleString(undefined, withTime
    ? { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" }
    : { day: "numeric", month: "short", year: "numeric" })
}

// Select is a native select styled like the inputs: few options, no search needed.
export function Select({ id, value, onChange, options }: { id?: string; value: string; onChange: (v: string) => void; options: { value: string; label: string }[] }) {
  return (
    <select
      id={id}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      className="h-8 w-full rounded-lg border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 dark:bg-input/30"
    >
      {options.map((o) => (
        <option key={o.value} value={o.value}>{o.label}</option>
      ))}
    </select>
  )
}
