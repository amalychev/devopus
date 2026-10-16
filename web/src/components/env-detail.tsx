import * as React from "react"
import { Code, ConnectError } from "@connectrpc/connect"
import {
  ExternalLinkIcon,
  MoonIcon,
  PlayIcon,
  SquareIcon,
  SunIcon,
  Trash2Icon,
} from "lucide-react"

import { Confirm } from "@/components/confirm"
import { DataView } from "@/components/data-view"
import { LogsView, type LogChoice } from "@/components/logs-view"
import { PhaseDot, phaseText } from "@/components/phase-dot"
import { Steps } from "@/components/steps"
import { UnitsTable } from "@/components/units-table"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { toast } from "@/components/ui/toast"
import { Phase, type EnvironmentStatus } from "@/gen/devopus/agent/v1/agent_pb"
import { agent, errorText } from "@/lib/api"
import {
  ago,
  date,
  envTitle,
  isFeature,
  phaseKey,
  phaseLabel,
} from "@/lib/format"

export function EnvDetail({
  env,
  onRemoved,
}: {
  env: EnvironmentStatus
  onRemoved: () => void
}) {
  const [tab, setTab] = React.useState("units")
  // The app remounts this per environment (key), which resets the tab and log.
  const [log, setLog] = React.useState<LogChoice>()

  const showLogs = (unit: string) => {
    setLog({ unit })
    setTab("logs")
  }

  const feature = isFeature(env)
  const hasCode = env.sources.length > 0
  return (
    <article className="flex flex-col gap-6">
      <Header env={env} onRemoved={onRemoved} />
      {env.error && env.phase === Phase.ERROR && (
        <Alert variant="destructive">
          <AlertTitle>The agent could not finish this environment</AlertTitle>
          <AlertDescription className="font-mono text-xs whitespace-pre-wrap">
            {env.error}
          </AlertDescription>
        </Alert>
      )}
      {env.merged && (
        <Alert>
          <AlertTitle>Branch {env.branch} is merged</AlertTitle>
          <AlertDescription>
            The work is in the base branch; this feature can be deleted.
          </AlertDescription>
        </Alert>
      )}
      <Steps steps={env.steps} />
      {env.notes.length > 0 && (
        <ul className="flex flex-col gap-1 text-sm text-muted-foreground">
          {env.notes.map((n) => (
            <li key={n}>{n}</li>
          ))}
        </ul>
      )}
      <Tabs value={tab} onValueChange={(v) => setTab(String(v))}>
        <TabsList variant="line">
          <TabsTrigger value="units">Modules and services</TabsTrigger>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          <TabsTrigger value="data">Data</TabsTrigger>
          {hasCode && <TabsTrigger value="code">Code</TabsTrigger>}
        </TabsList>
        <TabsContent value="units" className="pt-2">
          <UnitsTable env={env} onLogs={showLogs} />
          {feature && env.shared.length > 0 && (
            <p className="pt-4 text-sm text-muted-foreground">
              From main: {env.shared.join(", ")}. The feature reaches them by
              the same names.
            </p>
          )}
        </TabsContent>
        <TabsContent value="logs" className="pt-2">
          <LogsView env={env} choice={log} onChoose={setLog} />
        </TabsContent>
        <TabsContent value="data" className="pt-2">
          <DataView env={env} />
        </TabsContent>
        {hasCode && (
          <TabsContent value="code" className="pt-2">
            <CodeView env={env} />
          </TabsContent>
        )}
      </Tabs>
    </article>
  )
}

function Header({
  env,
  onRemoved,
}: {
  env: EnvironmentStatus
  onRemoved: () => void
}) {
  const phase = phaseKey(env.phase)
  const feature = isFeature(env)
  const lastActive = date(env.lastActive)
  const sleepingSince = date(env.sleepingSince)
  return (
    <header className="flex flex-col gap-3">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex min-w-0 flex-col gap-1">
          <h1 className="text-3xl font-semibold tracking-tight">
            {envTitle(env)}
          </h1>
          <p className="text-sm text-muted-foreground">
            {feature ? (
              <>
                Feature of {env.project} on{" "}
                <span className="font-mono text-foreground">{env.branch}</span>
                {env.forkedModules.length > 0 && (
                  <>, forks {env.forkedModules.join(", ")}</>
                )}
              </>
            ) : (
              <>Base environment of {env.project}</>
            )}
          </p>
        </div>
        <Actions env={env} onRemoved={onRemoved} />
      </div>
      <div className="flex flex-wrap items-center gap-x-5 gap-y-1 text-sm">
        <span
          className={`inline-flex items-center gap-2 font-medium ${phaseText(phase)}`}
        >
          <PhaseDot phase={phase} />
          {phaseLabel(env.phase)}
          {env.phase === Phase.SLEEPING && sleepingSince && (
            <span className="font-normal text-muted-foreground">
              {ago(sleepingSince)}
              {env.sleepReason && `, ${env.sleepReason}`}
            </span>
          )}
        </span>
        {env.url && (
          <a
            href={env.url}
            target="_blank"
            rel="noreferrer"
            className="inline-flex items-center gap-1.5 font-mono text-primary underline-offset-4 hover:underline"
          >
            {env.url.replace(/^https?:\/\//, "")}
            <ExternalLinkIcon className="size-3.5" aria-hidden />
          </a>
        )}
        {feature && lastActive && env.phase !== Phase.SLEEPING && (
          <span className="text-muted-foreground">
            Last used {ago(lastActive)}
          </span>
        )}
      </div>
    </header>
  )
}

function Actions({
  env,
  onRemoved,
}: {
  env: EnvironmentStatus
  onRemoved: () => void
}) {
  const [busy, setBusy] = React.useState<string>()
  const [confirm, setConfirm] = React.useState<"delete" | "stop">()
  const feature = isFeature(env)

  const act = async (
    what: string,
    fn: () => Promise<unknown>,
    done: string
  ) => {
    setBusy(what)
    try {
      await fn()
      toast.add({ title: done, type: "success" })
    } catch (err) {
      toast.add({
        title: `Could not ${what}`,
        description: errorText(err),
        type: "error",
      })
    } finally {
      setBusy(undefined)
    }
  }

  const name = envTitle(env)
  const stopped = env.phase === Phase.STOPPED
  return (
    <div className="flex flex-wrap gap-2">
      {feature && env.phase === Phase.RUNNING && (
        <Button
          variant="outline"
          disabled={!!busy}
          onClick={() =>
            act(
              "put it to sleep",
              () =>
                agent.setSleep({
                  project: env.project,
                  feature: env.id,
                  asleep: true,
                }),
              `${name} is going to sleep`
            )
          }
        >
          {busy ? (
            <Spinner data-icon="inline-start" />
          ) : (
            <MoonIcon data-icon="inline-start" />
          )}
          Sleep
        </Button>
      )}
      {feature && env.phase === Phase.SLEEPING && (
        <Button
          disabled={!!busy}
          onClick={() =>
            act(
              "wake it",
              () =>
                agent.setSleep({
                  project: env.project,
                  feature: env.id,
                  asleep: false,
                }),
              `${name} is waking up`
            )
          }
        >
          {busy ? (
            <Spinner data-icon="inline-start" />
          ) : (
            <SunIcon data-icon="inline-start" />
          )}
          Wake
        </Button>
      )}
      {!feature && !stopped && (
        <Button
          variant="outline"
          disabled={!!busy}
          onClick={() => setConfirm("stop")}
        >
          <SquareIcon data-icon="inline-start" />
          Stop
        </Button>
      )}
      {stopped && (
        <Button
          disabled={!!busy}
          onClick={() =>
            act(
              "start it",
              () => agent.up({ project: env.project }),
              `${env.project} is starting`
            )
          }
        >
          {busy ? (
            <Spinner data-icon="inline-start" />
          ) : (
            <PlayIcon data-icon="inline-start" />
          )}
          Start
        </Button>
      )}
      {feature && (
        <Button
          variant="destructive"
          disabled={!!busy}
          onClick={() => setConfirm("delete")}
        >
          <Trash2Icon data-icon="inline-start" />
          Delete
        </Button>
      )}
      <Confirm
        open={confirm === "stop"}
        onOpenChange={(o) => !o && setConfirm(undefined)}
        title={`Stop ${env.project}?`}
        description="Main and every feature of the project stop. Data, code and worktrees stay; Start brings them back."
        action="Stop"
        onConfirm={async () => {
          await agent.down({ project: env.project }).catch((err) => {
            throw new Error(errorText(err))
          })
        }}
      />
      <Confirm
        open={confirm === "delete"}
        onOpenChange={(o) => !o && setConfirm(undefined)}
        title={`Delete ${name}?`}
        description={`Its containers, databases and worktrees go away. Commits stay on branch ${env.branch}.`}
        action="Delete"
        onConfirm={async () => {
          try {
            await agent.removeFeature({ project: env.project, feature: env.id })
          } catch (err) {
            throw ConnectError.from(err)
          }
          onRemoved()
        }}
        retry={{
          action: "Delete anyway",
          when: (err) => {
            const ce = ConnectError.from(err)
            return ce.code === Code.FailedPrecondition
              ? ce.rawMessage
              : undefined
          },
          onConfirm: async () => {
            await agent
              .removeFeature({
                project: env.project,
                feature: env.id,
                force: true,
              })
              .catch((err) => {
                throw new Error(errorText(err))
              })
            onRemoved()
          },
        }}
      />
    </div>
  )
}

function CodeView({ env }: { env: EnvironmentStatus }) {
  return (
    <ul className="flex flex-col divide-y">
      {env.sources.map((s) => (
        <li key={s.module} className="flex flex-col gap-0.5 py-3">
          <span className="font-medium">
            {s.module}{" "}
            <span className="font-normal text-muted-foreground">
              {s.branch ? `on ${s.branch}` : "at the base branch"}
            </span>
          </span>
          <span className="font-mono text-xs break-all text-muted-foreground select-all">
            {s.path}
          </span>
        </li>
      ))}
    </ul>
  )
}
