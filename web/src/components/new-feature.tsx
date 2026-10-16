import * as React from "react"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import type {
  DescribeProjectResponse,
  FeaturePlan,
} from "@/gen/devopus/agent/v2/agent_pb"
import { agent, errorText } from "@/lib/api"

// slug is the agent's store.Slug; the agent adds a suffix if it is taken.
function slug(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 50)
    .replace(/-+$/, "")
}

// NewFeature is `devopus feature create` in the panel: a name, the applications to fork,
// and what the feature will run itself and take from main.
export function NewFeature({
  project,
  onClose,
  onCreated,
}: {
  project?: string
  onClose: () => void
  onCreated: (envId: string) => void
}) {
  return (
    <Dialog open={!!project} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">
        {project && (
          <NewFeatureForm
            key={project}
            project={project}
            onClose={onClose}
            onCreated={onCreated}
          />
        )}
      </DialogContent>
    </Dialog>
  )
}

function NewFeatureForm({
  project,
  onClose,
  onCreated,
}: {
  project: string
  onClose: () => void
  onCreated: (envId: string) => void
}) {
  const [info, setInfo] = React.useState<DescribeProjectResponse>()
  const [loadError, setLoadError] = React.useState<string>()
  const [name, setName] = React.useState("")
  const [forks, setForks] = React.useState<string[]>([])
  const [plan, setPlan] = React.useState<FeaturePlan>()
  const [busy, setBusy] = React.useState(false)
  const [error, setError] = React.useState<string>()

  React.useEffect(() => {
    agent
      .describeProject({ project })
      .then(setInfo)
      .catch((err) => setLoadError(errorText(err)))
  }, [project])

  React.useEffect(() => {
    if (forks.length === 0) return
    let stale = false
    agent
      .describeProject({ project, forks })
      .then((res) => !stale && setPlan(res.plan))
      .catch(() => !stale && setPlan(undefined))
    return () => {
      stale = true
    }
  }, [project, forks])

  const toggle = (m: string, on: boolean) =>
    setForks((prev) => (on ? [...prev, m].sort() : prev.filter((x) => x !== m)))

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      const res = await agent.createFeature({
        project,
        name: name.trim(),
        apps: forks,
      })
      onCreated(res.environmentId)
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const trimmed = name.trim()
  return (
    <form onSubmit={submit} className="flex flex-col gap-5">
      <DialogHeader>
        <DialogTitle>New feature of {project}</DialogTitle>
        <DialogDescription>
          The applications you fork get their own branch and worktree. Everything
          else comes from main.
        </DialogDescription>
      </DialogHeader>
      {loadError ? (
        <Alert variant="destructive">
          <AlertDescription className="whitespace-pre-wrap">
            {loadError}
          </AlertDescription>
        </Alert>
      ) : !info ? (
        <Spinner />
      ) : (
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="feature-name">Name</FieldLabel>
            <Input
              id="feature-name"
              autoFocus
              autoComplete="off"
              spellCheck={false}
              placeholder="PAY-123"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
            <FieldDescription>
              Branch{" "}
              <span className="font-mono">
                {info.branchPrefix}
                {trimmed || "name"}
              </span>
              {info.domain && (
                <>
                  {" "}
                  at{" "}
                  <span className="font-mono">
                    {slug(trimmed) || "name"}.{info.domain}
                  </span>
                </>
              )}
            </FieldDescription>
          </Field>
          <FieldSet>
            <FieldLegend variant="label">Applications to fork</FieldLegend>
            <div className="grid gap-2 sm:grid-cols-2">
              {info.apps.map((m) => (
                <Field key={m.name} orientation="horizontal">
                  <Checkbox
                    id={`fork-${m.name}`}
                    checked={forks.includes(m.name)}
                    onCheckedChange={(on) => toggle(m.name, on === true)}
                  />
                  <FieldLabel
                    htmlFor={`fork-${m.name}`}
                    className="font-normal"
                  >
                    {m.name}
                    {m.routes.length > 0 && (
                      <span className="font-mono text-xs text-muted-foreground">
                        {m.routes.join(" ")}
                      </span>
                    )}
                  </FieldLabel>
                </Field>
              ))}
            </div>
          </FieldSet>
          {forks.length > 0 && plan && <PlanSummary plan={plan} />}
        </FieldGroup>
      )}
      {error && (
        <Alert variant="destructive">
          <AlertDescription className="whitespace-pre-wrap">
            {error}
          </AlertDescription>
        </Alert>
      )}
      <DialogFooter>
        <Button type="button" variant="outline" onClick={onClose}>
          Cancel
        </Button>
        <Button
          type="submit"
          disabled={busy || !info || !trimmed || forks.length === 0}
        >
          {busy && <Spinner data-icon="inline-start" />}
          Create feature
        </Button>
      </DialogFooter>
    </form>
  )
}

function PlanSummary({ plan }: { plan: FeaturePlan }) {
  const rows: [string, string[], string][] = [
    [
      "Also runs",
      plan.dependents,
      "they use forked applications; on the base branch",
    ],
    [
      "Copies of main's data",
      plan.copies,
      "the feature's migrations run on the copy",
    ],
    ["Own, empty", plan.own, ""],
    ["From main", plan.shared, ""],
  ]
  return (
    <div className="flex flex-col gap-2 rounded-lg bg-muted/60 p-3 text-sm">
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
        {rows
          .filter(([, items]) => items.length > 0)
          .map(([label, items, hint]) => (
            <React.Fragment key={label}>
              <dt className="text-muted-foreground">{label}</dt>
              <dd>
                {items.join(", ")}
                {hint && (
                  <span className="text-muted-foreground"> — {hint}</span>
                )}
              </dd>
            </React.Fragment>
          ))}
      </dl>
      {plan.warnings.map((w) => (
        <p key={w} className="text-phase-creating">
          {w}
        </p>
      ))}
    </div>
  )
}
