import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Plus, Trash2 } from 'lucide-react'
import { toast } from 'sonner'
import { api, type InstanceSize, type SizeCatalog } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { cn } from '@/lib/utils'

const nameRe = /^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$/

/** Editable instance size catalog (cluster-wide). Changes apply to every
 * process using a size as soon as they are saved. */
export function SizesEditor() {
  const qc = useQueryClient()
  const catalog = useQuery({ queryKey: ['sizes'], queryFn: api.sizes })
  // Local edits; null means "not edited yet, mirror the server copy".
  const [edits, setEdits] = useState<SizeCatalog | null>(null)
  const draft = edits ?? (catalog.data ? structuredClone(catalog.data) : null)
  const setDraft = (c: SizeCatalog) => setEdits(c)
  const save = useMutation({
    mutationFn: (c: SizeCatalog) => api.saveSizes(c),
    onSuccess: (c) => {
      toast.success('Instance sizes saved; processes using changed sizes are being resized')
      qc.setQueryData(['sizes'], c)
      setEdits(null)
    },
    onError: (e: Error) => toast.error(e.message),
  })

  if (!draft) return <Skeleton className="h-48 w-full" />
  const dirty = JSON.stringify(draft) !== JSON.stringify(catalog.data)
  const invalid = draft.sizes.some((s) => !nameRe.test(s.name) || !s.cpu || !s.memory) || !draft.sizes.some((s) => s.name === draft.default)

  const update = (i: number, patch: Partial<InstanceSize>) =>
    setDraft({ ...draft, sizes: draft.sizes.map((s, j) => (j === i ? { ...s, ...patch } : s)) })
  const remove = (i: number) => setDraft({ ...draft, sizes: draft.sizes.filter((_, j) => j !== i) })
  const add = () => setDraft({ ...draft, sizes: [...draft.sizes, { name: '', kind: 'shared', cpu: '0.5', memory: '128Mi' }] })

  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-4">
        <div>
          <CardTitle>Instance sizes</CardTitle>
          <CardDescription>
            Named CPU and memory allocations a process runs with. <strong>Shared</strong> sizes get a guaranteed CPU share that can burst up to
            4×; <strong>dedicated</strong> sizes get whole cores with requests equal to limits. Memory is never overcommitted.
          </CardDescription>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="outline" onClick={add}>
            <Plus data-icon="inline-start" /> Add size
          </Button>
          <Button size="sm" disabled={!dirty || invalid || save.isPending} onClick={() => save.mutate(draft)}>
            Save
          </Button>
        </div>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-10">Default</TableHead>
              <TableHead>Name</TableHead>
              <TableHead>Kind</TableHead>
              <TableHead>CPU (cores)</TableHead>
              <TableHead>Memory</TableHead>
              <TableHead>Description</TableHead>
              <TableHead className="text-right"></TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {draft.sizes.map((s, i) => (
              <TableRow key={i}>
                <TableCell>
                  <input
                    type="radio"
                    name="default-size"
                    checked={draft.default === s.name && s.name !== ''}
                    onChange={() => setDraft({ ...draft, default: s.name })}
                    disabled={!nameRe.test(s.name)}
                    aria-label={`Make ${s.name} the default`}
                  />
                </TableCell>
                <TableCell>
                  <Input
                    value={s.name}
                    onChange={(e) => update(i, { name: e.target.value.toLowerCase() })}
                    className={cn('h-7 w-36 font-mono text-xs', !nameRe.test(s.name) && 'border-red-500')}
                    placeholder="shared-m"
                  />
                </TableCell>
                <TableCell>
                  <Select value={s.kind} onValueChange={(v) => update(i, { kind: v as InstanceSize['kind'] })}>
                    <SelectTrigger className="h-7 w-32 text-xs" size="sm">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="shared">shared</SelectItem>
                      <SelectItem value="dedicated">dedicated</SelectItem>
                    </SelectContent>
                  </Select>
                </TableCell>
                <TableCell>
                  <Input value={s.cpu} onChange={(e) => update(i, { cpu: e.target.value })} className="h-7 w-20 font-mono text-xs" placeholder="0.5" />
                </TableCell>
                <TableCell>
                  <Input value={s.memory} onChange={(e) => update(i, { memory: e.target.value })} className="h-7 w-24 font-mono text-xs" placeholder="64Mi" />
                </TableCell>
                <TableCell>
                  <Input value={s.description ?? ''} onChange={(e) => update(i, { description: e.target.value })} className="h-7 text-xs" placeholder="optional" />
                </TableCell>
                <TableCell className="text-right">
                  <Button
                    size="icon-xs"
                    variant="ghost"
                    className="text-destructive hover:text-destructive"
                    disabled={draft.default === s.name}
                    title={draft.default === s.name ? 'Pick another default first' : 'Remove'}
                    onClick={() => remove(i)}
                  >
                    <Trash2 />
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <p className="mt-3 text-xs text-muted-foreground">
          Processes pick a size in <code className="font-mono">shpyrd.yaml</code> (<code className="font-mono">processes.web.size: shared-m</code>), with{' '}
          <code className="font-mono">shpyrd resize web=shared-m</code>, or from the project page. Without one they get the default.
        </p>
      </CardContent>
    </Card>
  )
}
