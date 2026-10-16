import { cn } from "@/lib/utils"
import type { PhaseKey } from "@/lib/format"

const fill: Record<PhaseKey, string> = {
  running: "bg-phase-running border-phase-running",
  creating: "bg-phase-creating border-phase-creating",
  error: "bg-phase-error border-phase-error",
  // Asleep and stopped are hollow: nothing runs.
  sleeping: "bg-transparent border-phase-sleeping",
  stopped: "bg-transparent border-phase-stopped",
}

export function PhaseDot({
  phase,
  className,
}: {
  phase: PhaseKey
  className?: string
}) {
  return (
    <span
      aria-hidden
      className={cn(
        "relative inline-block size-2.5 shrink-0 rounded-full border-2",
        fill[phase],
        className
      )}
    >
      {phase === "creating" && (
        <span className="absolute -inset-1 animate-ping rounded-full border border-phase-creating" />
      )}
    </span>
  )
}

const text: Record<PhaseKey, string> = {
  running: "text-phase-running",
  creating: "text-phase-creating",
  error: "text-phase-error",
  sleeping: "text-phase-sleeping",
  stopped: "text-muted-foreground",
}

export function phaseText(p: PhaseKey) {
  return text[p]
}
