import { Field, FieldDescription, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

// TextField and AreaField are the form fields of the project's editors.
export function TextField({ id, label, value, onChange, hint, placeholder, mono, type }: {
  id: string; label: string; value: string; onChange: (v: string) => void; hint?: string; placeholder?: string; mono?: boolean; type?: string
}) {
  return (
    <Field>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <Input id={id} type={type} value={value} placeholder={placeholder} className={mono ? "font-mono" : undefined} onChange={(e) => onChange(e.target.value)} />
      {hint && <FieldDescription>{hint}</FieldDescription>}
    </Field>
  )
}

export function AreaField({ id, label, value, onChange, hint, placeholder, rows = 4 }: {
  id: string; label: string; value: string; onChange: (v: string) => void; hint?: string; placeholder?: string; rows?: number
}) {
  return (
    <Field>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <textarea
        id={id}
        rows={rows}
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        className="w-full rounded-lg border border-input bg-transparent px-2.5 py-1.5 font-mono text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 dark:bg-input/30"
      />
      {hint && <FieldDescription>{hint}</FieldDescription>}
    </Field>
  )
}
