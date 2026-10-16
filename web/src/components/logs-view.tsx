import * as React from "react"

import { Button } from "@/components/ui/button"
import { Field, FieldLabel } from "@/components/ui/field"
import { Switch } from "@/components/ui/switch"
import type {
  EnvironmentStatus,
  LogLine,
  LogSource,
} from "@/gen/devopus/agent/v1/agent_pb"
import { agent, errorText } from "@/lib/api"
import { cn } from "@/lib/utils"

const TAIL = 500
const KEEP = 5000

export interface LogChoice {
  unit?: string
  file?: string
}

function sameChoice(a: LogChoice | undefined, s: LogSource) {
  return !!a && (a.unit ? a.unit === s.unit : a.file === s.file)
}

function useLogSources(env: EnvironmentStatus) {
  const [sources, setSources] = React.useState<LogSource[]>([])
  // Units come and go with the environment; hooks write new files as they run.
  const unitKey = env.units.map((u) => `${u.name}:${u.state}`).join(",")
  const stepKey = env.steps.map((s) => s.state).join(",")
  React.useEffect(() => {
    let stopped = false
    agent
      .logSources({ environmentId: env.id })
      .then((res) => !stopped && setSources(res.sources))
      .catch(() => {})
    return () => {
      stopped = true
    }
  }, [env.id, unitKey, stepKey])
  return sources
}

// useLogLines streams one source; LogsView remounts LogPane per source (key), which
// starts it with empty lines.
function useLogLines(envId: string, choice: LogChoice, follow: boolean) {
  const [lines, setLines] = React.useState<LogLine[]>([])
  const [error, setError] = React.useState<string>()
  React.useEffect(() => {
    const abort = new AbortController()
    ;(async () => {
      try {
        for await (const res of agent.logs(
          { environmentId: envId, source: choice, tail: TAIL, follow },
          { signal: abort.signal }
        )) {
          setLines((prev) => {
            const next = prev.concat(res.lines)
            return next.length > KEEP ? next.slice(next.length - KEEP) : next
          })
        }
      } catch (err) {
        if (!abort.signal.aborted) setError(errorText(err))
      }
    })()
    return () => abort.abort()
  }, [envId, choice.unit, choice.file, follow]) // eslint-disable-line react-hooks/exhaustive-deps
  return { lines, error }
}

export function LogsView({
  env,
  choice,
  onChoose,
}: {
  env: EnvironmentStatus
  choice?: LogChoice
  onChoose: (c: LogChoice) => void
}) {
  const sources = useLogSources(env)
  const [follow, setFollow] = React.useState(true)
  const current =
    choice ??
    (sources[0] ? { unit: sources[0].unit, file: sources[0].file } : undefined)

  const units = sources.filter((s) => s.unit)
  const files = sources.filter((s) => s.file)

  return (
    <div className="grid min-h-0 gap-4 md:grid-cols-[12rem_1fr]">
      <div className="flex flex-col gap-4">
        <SourceList
          title="Modules and services"
          items={units}
          current={current}
          onChoose={onChoose}
        />
        <SourceList
          title="Hooks and builds"
          items={files}
          current={current}
          onChoose={onChoose}
        />
        {sources.length === 0 && (
          <p className="text-sm text-muted-foreground">No output yet.</p>
        )}
      </div>
      <div className="flex min-w-0 flex-col gap-2">
        <div className="flex items-center justify-between gap-4">
          <span className="truncate text-sm font-medium">
            {current?.unit ?? current?.file ?? ""}
          </span>
          <Field orientation="horizontal" className="w-fit">
            <Switch id="follow" checked={follow} onCheckedChange={setFollow} />
            <FieldLabel htmlFor="follow">Follow</FieldLabel>
          </Field>
        </div>
        {current && (
          <LogPane
            key={`${current.unit}|${current.file}|${follow}`}
            envId={env.id}
            choice={current}
            follow={follow}
          />
        )}
      </div>
    </div>
  )
}

function LogPane({
  envId,
  choice,
  follow,
}: {
  envId: string
  choice: LogChoice
  follow: boolean
}) {
  const { lines, error } = useLogLines(envId, choice, follow)
  return (
    <>
      <LogLines lines={lines} follow={follow} />
      {error && <p className="text-sm text-phase-error">{error}</p>}
    </>
  )
}

function SourceList({
  title,
  items,
  current,
  onChoose,
}: {
  title: string
  items: LogSource[]
  current?: LogChoice
  onChoose: (c: LogChoice) => void
}) {
  if (items.length === 0) return null
  return (
    <div className="flex flex-col gap-1">
      <h3 className="px-2 text-xs font-medium text-muted-foreground">
        {title}
      </h3>
      <ul className="flex flex-col">
        {items.map((s) => {
          const name = s.unit || s.file
          const active = sameChoice(current, s)
          return (
            <li key={`${s.unit}|${s.file}`}>
              <Button
                variant={active ? "secondary" : "ghost"}
                size="sm"
                aria-pressed={active}
                className="w-full justify-start font-normal"
                onClick={() =>
                  onChoose(s.unit ? { unit: s.unit } : { file: s.file })
                }
              >
                <span className="truncate">{name}</span>
              </Button>
            </li>
          )
        })}
      </ul>
    </div>
  )
}

const timeFmt = new Intl.DateTimeFormat(undefined, {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
})

function LogLines({ lines, follow }: { lines: LogLine[]; follow: boolean }) {
  const ref = React.useRef<HTMLDivElement>(null)
  const pinned = React.useRef(true)
  // Stay at the bottom while new lines arrive, unless the reader scrolled up.
  React.useLayoutEffect(() => {
    const el = ref.current
    if (el && follow && pinned.current) el.scrollTop = el.scrollHeight
  }, [lines, follow])
  return (
    <div
      ref={ref}
      onScroll={(e) => {
        const el = e.currentTarget
        pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
      }}
      className="h-[min(60vh,40rem)] overflow-auto rounded-lg border bg-card p-3 font-mono text-xs leading-5"
      role="log"
      aria-live="off"
    >
      {lines.length === 0 ? (
        <span className="text-muted-foreground">Waiting for output…</span>
      ) : (
        lines.map((l, i) => (
          // Many servers log every request to stderr: a mark in the gutter, not
          // a red line, tells the streams apart.
          <div
            key={i}
            className={cn(
              "-ml-3 flex gap-3 border-l-2 pl-2.5 break-all whitespace-pre-wrap",
              l.stderr ? "border-phase-creating/70" : "border-transparent"
            )}
            title={l.stderr ? "stderr" : undefined}
          >
            {l.time && (
              <span className="shrink-0 text-muted-foreground select-none">
                {timeFmt.format(new Date(Number(l.time.seconds) * 1000))}
              </span>
            )}
            <span>{l.text}</span>
          </div>
        ))
      )}
    </div>
  )
}
