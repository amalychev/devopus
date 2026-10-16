import * as React from "react"
import { MonitorIcon, MoonIcon, SunIcon } from "lucide-react"

import { EnvDetail } from "@/components/env-detail"
import { EnvTree, groupByProject } from "@/components/env-tree"
import { NewFeature } from "@/components/new-feature"
import { useTheme } from "@/components/theme-provider"
import { Button } from "@/components/ui/button"
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Toaster } from "@/components/ui/toast"
import { TooltipProvider } from "@/components/ui/tooltip"
import type { StatusResponse } from "@/gen/devopus/agent/v1/agent_pb"
import { useAgent, type Connection } from "@/hooks/use-agent"
import { isFeature } from "@/lib/format"

// The selected environment lives in the URL hash, so a reload or a second tab shows
// the same one.
function useSelected(): [string | undefined, (id?: string) => void] {
  const read = () =>
    decodeURIComponent(window.location.hash.replace(/^#\/?/, "")) || undefined
  const [id, setId] = React.useState(read)
  React.useEffect(() => {
    const on = () => setId(read())
    window.addEventListener("hashchange", on)
    return () => window.removeEventListener("hashchange", on)
  }, [])
  const select = React.useCallback((next?: string) => {
    window.location.hash = next ? `/${encodeURIComponent(next)}` : ""
    setId(next)
  }, [])
  return [id, select]
}

export function App() {
  const { connection, status, envs } = useAgent()
  const [selected, select] = useSelected()
  const [newFeatureFor, setNewFeatureFor] = React.useState<string>()

  const groups = React.useMemo(
    () => groupByProject(envs, status?.projects.map((p) => p.name) ?? []),
    [envs, status]
  )
  const current = envs.find((e) => e.id === selected)

  // With nothing chosen, open main of the first project.
  React.useEffect(() => {
    if (!selected && connection === "live") {
      const first = groups.find((g) => g.base)?.base
      if (first) select(first.id)
    }
  }, [selected, connection, groups, select])

  if (connection === "signed-out") return <SignedOut />

  return (
    <TooltipProvider>
      <div className="grid min-h-svh md:grid-cols-[17rem_1fr]">
        <aside className="flex flex-col gap-6 border-b bg-sidebar px-3 py-5 md:sticky md:top-0 md:h-svh md:overflow-y-auto md:border-r md:border-b-0">
          <div className="flex items-center justify-between px-3">
            <span className="text-lg font-semibold tracking-tight">
              Devopus
            </span>
            <ThemeToggle />
          </div>
          {connection === "connecting" ? (
            <div className="flex flex-col gap-2 px-3">
              <Skeleton className="h-4 w-24" />
              <Skeleton className="h-7 w-full" />
              <Skeleton className="h-7 w-full" />
            </div>
          ) : groups.length === 0 ? (
            <p className="px-3 text-sm text-sidebar-foreground/70">
              No projects yet. Run <code className="font-mono">devopus up</code>{" "}
              in a folder with workspace.yaml.
            </p>
          ) : (
            <EnvTree
              groups={groups}
              selected={selected}
              onSelect={select}
              onNewFeature={setNewFeatureFor}
            />
          )}
          <DeviceStatus status={status} connection={connection} />
        </aside>
        <main className="min-w-0 px-4 py-6 md:px-10 md:py-8">
          <div className="mx-auto max-w-5xl">
            {current ? (
              <EnvDetail
                key={current.id}
                env={current}
                onRemoved={() => {
                  const base = envs.find(
                    (e) => e.project === current.project && !isFeature(e)
                  )
                  select(base?.id)
                }}
              />
            ) : connection === "connecting" ? (
              <Skeleton className="h-10 w-64" />
            ) : (
              <Empty>
                <EmptyHeader>
                  <EmptyTitle>
                    {selected ? "This environment is gone" : "Nothing selected"}
                  </EmptyTitle>
                  <EmptyDescription>
                    Choose an environment on the left.
                  </EmptyDescription>
                </EmptyHeader>
              </Empty>
            )}
          </div>
        </main>
      </div>
      <NewFeature
        project={newFeatureFor}
        onClose={() => setNewFeatureFor(undefined)}
        onCreated={(id) => {
          setNewFeatureFor(undefined)
          select(id)
        }}
      />
      <Toaster />
    </TooltipProvider>
  )
}

// DeviceStatus is what all projects share: the agent, the container runtime and the proxy.
function DeviceStatus({
  status,
  connection,
}: {
  status?: StatusResponse
  connection: Connection
}) {
  const rt = status?.runtime
  const proxy = status?.proxy
  const lines: { text: string; bad?: boolean }[] = []
  if (connection === "lost")
    lines.push({
      text: "The agent does not answer; reconnecting… Start it with devopus agent start",
      bad: true,
    })
  if (rt) {
    lines.push(
      rt.reachable
        ? { text: `${rt.name} ${rt.serverVersion}` }
        : { text: rt.error || `${rt.name} is not reachable`, bad: true }
    )
  }
  if (proxy?.state) {
    const ports =
      proxy.httpsPort && proxy.httpsPort !== 443
        ? `, HTTPS on ${proxy.httpsPort}`
        : ""
    lines.push(
      proxy.error
        ? { text: `Proxy: ${proxy.error}`, bad: true }
        : { text: `Proxy ${proxy.state}${ports}` }
    )
  }
  if (status?.agentVersion) lines.push({ text: `Agent ${status.agentVersion}` })
  return (
    <footer className="mt-auto flex flex-col gap-0.5 px-3 text-xs text-sidebar-foreground/60">
      {lines.map((l) => (
        <span key={l.text} className={l.bad ? "text-phase-error" : undefined}>
          {l.text}
        </span>
      ))}
    </footer>
  )
}

function ThemeToggle() {
  const { theme, setTheme } = useTheme()
  const next =
    theme === "light" ? "dark" : theme === "dark" ? "system" : "light"
  const Icon =
    theme === "light" ? SunIcon : theme === "dark" ? MoonIcon : MonitorIcon
  return (
    <Button
      variant="ghost"
      size="icon-sm"
      onClick={() => setTheme(next)}
      aria-label={`Theme: ${theme}. Switch to ${next}`}
    >
      <Icon />
    </Button>
  )
}

function SignedOut() {
  return (
    <main className="grid min-h-svh place-items-center px-4">
      <Empty>
        <EmptyHeader>
          <EmptyTitle>Open the panel from your terminal</EmptyTitle>
          <EmptyDescription>
            The panel controls containers and code on this machine, so it opens
            only with a link from the devopus command. Run{" "}
            <code className="font-mono text-foreground">devopus panel open</code>.
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    </main>
  )
}

export default App
