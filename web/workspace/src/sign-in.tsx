import * as React from "react"

import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { auth, errorText, type Plan } from "@ws/api"
import { Select } from "@ws/ui"

// SignIn is 'devopus login --email' and 'devopus register' on a page: an email, the
// one-time code sent to it, and for a new account the plan.
export function SignIn() {
  const [register, setRegister] = React.useState(false)
  const [email, setEmail] = React.useState("")
  const [challenge, setChallenge] = React.useState<string>()
  const [code, setCode] = React.useState("")
  const [plans, setPlans] = React.useState<Plan[]>([])
  const [plan, setPlan] = React.useState(0)
  const [busy, setBusy] = React.useState(false)
  const [error, setError] = React.useState<string>()

  React.useEffect(() => {
    if (register && plans.length === 0) {
      auth.plans().then(setPlans).catch(() => undefined)
    }
  }, [register, plans.length])

  const run = async (fn: () => Promise<void>) => {
    setBusy(true)
    setError(undefined)
    try {
      await fn()
    } catch (err) {
      setError(errorText(err))
    } finally {
      setBusy(false)
    }
  }

  const send = (e: React.FormEvent) => {
    e.preventDefault()
    run(async () => {
      const res = await auth.start(email.trim(), register)
      setChallenge(res.challengeId)
      setCode("")
    })
  }

  const verify = (e: React.FormEvent) => {
    e.preventDefault()
    const chosen = plan || plans.find((p) => p.available)?.id || 1
    run(async () => {
      await auth.complete(challenge!, code.trim(), chosen)
    })
  }

  const available = plans.filter((p) => p.available)
  const coming = plans.filter((p) => !p.available)

  return (
    <main className="grid min-h-svh place-items-center px-4">
      <div className="flex w-full max-w-sm flex-col gap-6">
        <div className="flex flex-col gap-1">
          <span className="text-lg font-semibold tracking-tight">Devopus</span>
          <h1 className="text-2xl font-semibold tracking-tight">
            {register ? "Create an account" : "Sign in"}
          </h1>
          <p className="text-sm text-muted-foreground">
            {challenge
              ? `Enter the code sent to ${email.trim()}. It works for 10 minutes.`
              : "We email you a one-time code; there are no passwords."}
          </p>
        </div>
        {error && (
          <Alert variant="destructive">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        {!challenge ? (
          <form onSubmit={send}>
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="email">Email</FieldLabel>
                <Input
                  id="email"
                  type="email"
                  autoComplete="email"
                  autoFocus
                  required
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                />
              </Field>
              {register && available.length > 0 && (
                <Field>
                  <FieldLabel htmlFor="plan">Plan</FieldLabel>
                  <Select id="plan" value={String(plan || available[0].id)} onChange={(v) => setPlan(Number(v))}
                    options={available.map((p) => ({ value: String(p.id), label: p.name }))} />
                  <FieldDescription>
                    {available.find((p) => p.id === (plan || available[0].id))?.description}
                    {coming.length > 0 && ` · ${coming.map((p) => p.name).join(" and ")} coming`}
                  </FieldDescription>
                </Field>
              )}
              <Button type="submit" disabled={busy || !email.trim()}>
                {busy && <Spinner />}
                Send code
              </Button>
            </FieldGroup>
          </form>
        ) : (
          <form onSubmit={verify}>
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="code">Code</FieldLabel>
                <Input
                  id="code"
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  autoFocus
                  required
                  maxLength={7}
                  className="font-mono tracking-[0.3em]"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                />
              </Field>
              <Button type="submit" disabled={busy || code.trim().length < 6}>
                {busy && <Spinner />}
                {register ? "Create account" : "Sign in"}
              </Button>
              <Button type="button" variant="ghost" onClick={() => setChallenge(undefined)}>
                Use another email
              </Button>
            </FieldGroup>
          </form>
        )}
        <p className="text-sm text-muted-foreground">
          {register ? "Have an account? " : "New to Devopus? "}
          <button
            type="button"
            className="font-medium text-foreground underline-offset-4 hover:underline"
            onClick={() => {
              setRegister(!register)
              setChallenge(undefined)
              setError(undefined)
            }}
          >
            {register ? "Sign in" : "Create an account"}
          </button>
        </p>
      </div>
    </main>
  )
}
