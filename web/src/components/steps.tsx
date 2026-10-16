import { CheckIcon, XIcon } from "lucide-react"

import { Spinner } from "@/components/ui/spinner"
import { StepState, type StepProgress } from "@/gen/devopus/agent/v2/agent_pb"
import { date, duration } from "@/lib/format"
import { cn } from "@/lib/utils"

// Steps is the agent's way through an environment, in order: what is done, what it
// is doing now and what is left.
export function Steps({ steps }: { steps: StepProgress[] }) {
  if (steps.length === 0) return null
  const current = steps.find(
    (s) => s.state === StepState.RUNNING || s.state === StepState.FAILED
  )
  return (
    <div className="flex flex-col gap-2">
      <ol className="flex flex-wrap items-center gap-1.5">
        {steps.map((s) => (
          <li key={s.name}>
            <Step step={s} />
          </li>
        ))}
      </ol>
      {current?.message && (
        <p
          className={cn(
            "text-sm",
            current.state === StepState.FAILED
              ? "text-phase-error"
              : "text-muted-foreground"
          )}
        >
          <span className="font-medium text-foreground">{current.name}</span>:{" "}
          {current.message}
        </p>
      )}
    </div>
  )
}

function Step({ step }: { step: StepProgress }) {
  const took = duration(date(step.startedAt), date(step.finishedAt))
  const state = step.state
  return (
    <span
      title={took ? `${step.name}: ${took}` : step.name}
      className={cn(
        "inline-flex h-7 items-center gap-1.5 rounded-full border px-2.5 text-xs font-medium",
        state === StepState.DONE &&
          "border-transparent bg-phase-running/12 text-foreground",
        state === StepState.RUNNING &&
          "border-phase-creating bg-phase-creating/10 text-foreground",
        state === StepState.FAILED &&
          "border-phase-error bg-phase-error/10 text-phase-error",
        state === StepState.PENDING &&
          "border-dashed border-border text-muted-foreground",
        state === StepState.UNSPECIFIED && "border-border text-muted-foreground"
      )}
    >
      {state === StepState.DONE && (
        <CheckIcon className="size-3.5 text-phase-running" aria-label="done" />
      )}
      {state === StepState.RUNNING && (
        <Spinner className="size-3.5 text-phase-creating" />
      )}
      {state === StepState.FAILED && (
        <XIcon className="size-3.5" aria-label="failed" />
      )}
      {step.name}
    </span>
  )
}
