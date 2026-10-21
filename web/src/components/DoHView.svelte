<script lang="ts">
  // DoH Resolvers page: the dedicated home for DNS-over-HTTPS pools.
  // A doh pool used to live only as a type inside the Proxy Pools form;
  // everything here works through the same dashboard APIs that form uses
  // (pools CRUD/test, settings PATCH for provider pinning), plus the two
  // DoH-specific reads: the backend-owned defaults (seed list + alias
  // table) and the per-resolver probe the aggregate pool test cannot give.
  import { onMount } from 'svelte'
  import Badge from '../lib/ui/Badge.svelte'
  import Button from '../lib/ui/Button.svelte'
  import Card from '../lib/ui/Card.svelte'
  import Input from '../lib/ui/Input.svelte'
  import Toggle from '../lib/ui/Toggle.svelte'
  import {
    api,
    type DoHDefaults,
    type DoHResolverProbeResult,
    type ProviderStrategyConfig,
    type ProxyPool,
    type Settings
  } from '../api/client'
  import { notifications } from '../lib/notifications'
  import { getLatencyBadge, getStatusVariant } from './proxypools/helpers'

  // The provider this page's pin control manages: the reference install
  // routes its OpenCode traffic through the DoH pool, and the pin section
  // below is the one place that assignment is flipped.
  const OPENCODE_PROVIDER = 'opencode'

  let loading = $state(true)
  let pools = $state<ProxyPool[]>([])
  let settings = $state<Settings | null>(null)
  let defaults = $state<DoHDefaults | null>(null)
  let selectedPoolId = $state<string | null>(null)
  let creating = $state(false)
  let savingUrls = $state(false)
  let probing = $state(false)
  let pinBusy = $state(false)
  let probeResults = $state<Record<string, DoHResolverProbeResult>>({})
  let probedAt = $state<string | null>(null)
  let newResolver = $state('')
  let resolverError = $state('')

  let dohPools = $derived(pools.filter((pool) => pool.type === 'doh'))
  let selectedPool = $derived(
    dohPools.find((pool) => pool.id === selectedPoolId) ??
      dohPools.find((pool) => pool.name === defaults?.poolName) ??
      dohPools[0] ??
      null
  )
  let resolvers = $derived(selectedPool?.urls ?? [])
  let poolStrategy = $derived(
    typeof selectedPool?.strategy === 'string' ? selectedPool.strategy : 'round-robin'
  )
  let strategies = $derived(settings?.providerStrategies ?? {})
  let pinnedProviders = $derived(
    selectedPool
      ? Object.entries(strategies)
          .filter(([, strategy]) => strategy?.proxyPoolId === selectedPool.id)
          .map(([provider]) => provider)
      : []
  )
  let opencodePinned = $derived(
    !!selectedPool && strategies[OPENCODE_PROVIDER]?.proxyPoolId === selectedPool.id
  )
  let probeSummary = $derived.by(() => {
    const results = Object.values(probeResults)
    if (results.length === 0) return null
    return {
      ok: results.filter((result) => result.ok).length,
      total: results.length
    }
  })

  function formatDateTime(value: string | null | undefined): string {
    if (!value) return 'Never'
    const date = new Date(value)
    if (Number.isNaN(date.getTime())) return 'Never'
    return date.toLocaleString()
  }

  // The alias → endpoint mapping is the server's (GET /api/doh/defaults),
  // so what this page shows next to a spec is what the data plane dials.
  function endpointFor(spec: string): string {
    return defaults?.aliases?.[spec.trim().toLowerCase()] ?? spec
  }

  function isAlias(spec: string): boolean {
    return !!defaults?.aliases?.[spec.trim().toLowerCase()]
  }

  async function loadData() {
    const [poolData, settingsData, defaultsData] = await Promise.all([
      api.getProxyPools(false),
      api.getSettings().catch(() => null),
      api.getDoHDefaults().catch(() => null)
    ])
    pools = poolData
    settings = settingsData
    if (defaultsData) defaults = defaultsData
    if (selectedPoolId && !poolData.some((pool) => pool.id === selectedPoolId)) {
      selectedPoolId = null
    }
  }

  onMount(async () => {
    try {
      await loadData()
    } finally {
      loading = false
    }
  })

  async function handleCreatePool() {
    if (!defaults || creating) return
    creating = true
    try {
      const created = await api.createProxyPool({
        name: defaults.poolName,
        type: 'doh',
        urls: [...defaults.resolvers],
        strictProxy: true,
        isActive: true
      })
      selectedPoolId = created.id
      probeResults = {}
      await loadData()
      notifications.success('DoH pool created with the default resolver list')
    } catch (err) {
      notifications.error(err instanceof Error ? err.message : 'Failed to create DoH pool')
    } finally {
      creating = false
    }
  }

  async function saveResolvers(next: string[]) {
    if (!selectedPool || savingUrls) return
    savingUrls = true
    try {
      await api.updateProxyPool(selectedPool.id, { type: 'doh', urls: next })
      probeResults = {}
      probedAt = null
      await loadData()
      notifications.success('Resolver list updated')
    } catch (err) {
      notifications.error(err instanceof Error ? err.message : 'Failed to update resolvers')
    } finally {
      savingUrls = false
    }
  }

  // One validity rule for a resolver spec, mirroring the data plane: a
  // known alias, or an https URL with a host. Duplicates are judged on the
  // resolved endpoint, so "cloudflare" and its URL cannot both sneak in.
  function validateResolver(spec: string): string | null {
    const trimmed = spec.trim()
    if (!trimmed) return 'Resolver is required'
    if (isAlias(trimmed)) {
      const endpoint = endpointFor(trimmed)
      if (resolvers.some((existing) => endpointFor(existing) === endpoint)) {
        return 'That resolver is already in the pool'
      }
      return null
    }
    if (!trimmed.startsWith('https://')) {
      return 'Use a known alias or an https:// DoH endpoint URL'
    }
    try {
      if (!new URL(trimmed).hostname) return 'The endpoint URL has no host'
    } catch {
      return 'The endpoint URL is not valid'
    }
    if (resolvers.some((existing) => endpointFor(existing) === trimmed)) {
      return 'That resolver is already in the pool'
    }
    return null
  }

  async function handleAddResolver() {
    const spec = newResolver.trim()
    const error = validateResolver(spec)
    if (error) {
      resolverError = error
      return
    }
    resolverError = ''
    await saveResolvers([...resolvers, spec])
    if (!savingUrls) newResolver = ''
  }

  async function handleRemoveResolver(spec: string) {
    if (resolvers.length <= 1) {
      notifications.error('A DoH pool needs at least one resolver')
      return
    }
    await saveResolvers(resolvers.filter((existing) => existing !== spec))
  }

  async function handleTestAll() {
    if (!selectedPool || probing) return
    probing = true
    try {
      // The sweep grades each resolver on its own; the aggregate test runs
      // alongside so the pool's stored verdict (status card) also refreshes.
      const [sweep] = await Promise.all([
        api.testProxyPoolResolvers(selectedPool.id),
        api.testProxyPool(selectedPool.id).catch(() => null)
      ])
      probeResults = Object.fromEntries(sweep.results.map((result) => [result.spec, result]))
      probedAt = new Date().toLocaleString()
      const ok = sweep.results.filter((result) => result.ok).length
      if (ok === sweep.results.length) {
        notifications.success(`All ${ok} resolvers answered`)
      } else {
        notifications.warning(`${ok} of ${sweep.results.length} resolvers answered`)
      }
      await loadData()
    } catch (err) {
      notifications.error(err instanceof Error ? err.message : 'Resolver test failed')
    } finally {
      probing = false
    }
  }

  async function handleToggleActive() {
    if (!selectedPool) return
    try {
      await api.updateProxyPool(selectedPool.id, { isActive: !selectedPool.isActive })
      await loadData()
    } catch (err) {
      notifications.error(err instanceof Error ? err.message : 'Failed to update pool')
    }
  }

  async function handleToggleStrict() {
    if (!selectedPool) return
    try {
      await api.updateProxyPool(selectedPool.id, { strictProxy: selectedPool.strictProxy !== true })
      await loadData()
    } catch (err) {
      notifications.error(err instanceof Error ? err.message : 'Failed to update pool')
    }
  }

  // Pin/unpin follows the settings pattern the provider pages use: read
  // the strategies map, change only the opencode entry's proxyPoolId
  // (keeping its rotateStrategy and every other provider untouched), and
  // PATCH the whole map back.
  async function setOpencodePin(pin: boolean) {
    if (!selectedPool || pinBusy) return
    pinBusy = true
    try {
      const current = settings?.providerStrategies ?? {}
      const updated: Record<string, ProviderStrategyConfig> = { ...current }
      const entry: ProviderStrategyConfig = { ...(updated[OPENCODE_PROVIDER] ?? {}) }
      if (pin) {
        entry.proxyPoolId = selectedPool.id
      } else {
        delete entry.proxyPoolId
      }
      if (Object.keys(entry).length === 0) {
        delete updated[OPENCODE_PROVIDER]
      } else {
        updated[OPENCODE_PROVIDER] = entry
      }
      await api.patchSettings({ providerStrategies: updated })
      if (settings) settings.providerStrategies = updated
      notifications.success(
        pin ? `Pinned ${OPENCODE_PROVIDER} to ${selectedPool.name}` : `Unpinned ${OPENCODE_PROVIDER}`
      )
    } catch (err) {
      notifications.error(err instanceof Error ? err.message : 'Failed to update provider pin')
    } finally {
      pinBusy = false
    }
  }
</script>

{#if loading}
  <div class="mx-auto flex w-full max-w-5xl flex-col gap-4 px-1 sm:gap-6 sm:px-0">
    <div class="h-20 rounded-[14px] bg-surface border border-border-subtle animate-pulse"></div>
    <div class="h-64 rounded-[14px] bg-surface border border-border-subtle animate-pulse"></div>
  </div>
{:else if dohPools.length === 0}
  <div class="mx-auto flex w-full max-w-5xl flex-col gap-4 px-1 sm:gap-6 sm:px-0">
    <Card>
      <div class="flex flex-col items-center gap-3 py-8 text-center">
        <span class="material-symbols-outlined text-[40px] text-text-muted">dns</span>
        <p class="font-semibold text-lg">No DoH pool yet</p>
        <p class="max-w-md text-sm text-text-muted">
          A DoH pool resolves upstream hostnames over DNS-over-HTTPS instead of the system
          resolver. Create one with the verified default resolver list
          {#if defaults}({defaults.resolvers.length} resolvers){/if}, then pin a provider to it
          below.
        </p>
        <Button onclick={handleCreatePool} disabled={!defaults || creating}>
          {creating ? 'Creating…' : 'Buat pool DoH'}
        </Button>
        {#if !defaults}
          <p class="text-xs text-text-muted">
            The server did not return the DoH defaults, so the pool cannot be created from here.
          </p>
        {/if}
      </div>
    </Card>
  </div>
{:else if selectedPool}
  <div class="mx-auto flex w-full max-w-5xl flex-col gap-4 px-1 sm:gap-6 sm:px-0">
    {#if dohPools.length > 1}
      <div class="flex items-center gap-3">
        <label class="text-sm font-medium" for="doh-pool-select">Pool</label>
        <select
          id="doh-pool-select"
          class="py-2 px-3 text-sm text-text-main bg-surface-2 border border-transparent rounded-[10px] focus:ring-1 focus:ring-primary/30 focus:border-primary/50 focus:outline-none transition-all"
          value={selectedPool.id}
          onchange={(event) => {
            selectedPoolId = event.currentTarget.value
            probeResults = {}
            probedAt = null
          }}
        >
          {#each dohPools as pool (pool.id)}
            <option value={pool.id}>{pool.name}</option>
          {/each}
        </select>
      </div>
    {/if}

    <Card title={selectedPool.name} subtitle="DNS-over-HTTPS resolver pool">
      {#snippet icon()}
        <span class="material-symbols-outlined text-[20px] text-text-muted">dns</span>
      {/snippet}
      <div class="grid grid-cols-2 gap-4 sm:grid-cols-3">
        <div>
          <p class="text-xs text-text-muted">Status</p>
          <div class="mt-1">
            <Badge variant={selectedPool.isActive ? 'success' : 'default'}>
              {selectedPool.isActive ? 'Active' : 'Inactive'}
            </Badge>
          </div>
        </div>
        <div>
          <p class="text-xs text-text-muted">Strategy</p>
          <p class="mt-1 text-sm font-medium">{poolStrategy}</p>
        </div>
        <div>
          <p class="text-xs text-text-muted">Resolvers</p>
          <p class="mt-1 text-sm font-medium">{resolvers.length}</p>
        </div>
        <div>
          <p class="text-xs text-text-muted">Last pool test</p>
          <div class="mt-1 flex items-center gap-2">
            <Badge variant={getStatusVariant(selectedPool.testStatus)} size="sm" dot>
              {selectedPool.testStatus || 'unknown'}
            </Badge>
            {#if getLatencyBadge(selectedPool.latency)}
              <Badge variant={getLatencyBadge(selectedPool.latency)?.variant} size="sm">
                {getLatencyBadge(selectedPool.latency)?.text}
              </Badge>
            {/if}
          </div>
        </div>
        <div>
          <p class="text-xs text-text-muted">Last tested at</p>
          <p class="mt-1 text-sm font-medium">{formatDateTime(selectedPool.lastTestedAt)}</p>
        </div>
        <div>
          <p class="text-xs text-text-muted">Bound connections</p>
          <p class="mt-1 text-sm font-medium">{selectedPool.boundConnectionCount ?? 0}</p>
        </div>
      </div>
      <div class="mt-5 flex flex-col gap-3">
        <div
          class="flex flex-col gap-3 rounded-lg border border-border/50 p-3 sm:flex-row sm:items-center sm:justify-between"
        >
          <div>
            <p class="font-medium text-sm">Active</p>
            <p class="text-xs text-text-muted">Inactive pools are ignored by runtime resolution.</p>
          </div>
          <Toggle checked={selectedPool.isActive === true} onChange={handleToggleActive} />
        </div>
        <div
          class="flex flex-col gap-3 rounded-lg border border-border/50 p-3 sm:flex-row sm:items-center sm:justify-between"
        >
          <div>
            <p class="font-medium text-sm">Strict Proxy</p>
            <p class="text-xs text-text-muted">
              Fail requests when no resolver answers instead of falling back to direct.
            </p>
          </div>
          <Toggle checked={selectedPool.strictProxy === true} onChange={handleToggleStrict} />
        </div>
      </div>
    </Card>

    <Card title="Resolvers" subtitle="Rotation order is the list order; a query fails over to the next resolver">
      {#snippet action()}
        <Button size="sm" onclick={handleTestAll} disabled={probing || savingUrls}>
          {probing ? 'Testing…' : 'Test semua resolver'}
        </Button>
      {/snippet}
      {#if probeSummary}
        <p class="mb-3 text-xs text-text-muted">
          Last sweep {probedAt}: {probeSummary.ok} of {probeSummary.total} resolvers answered.
        </p>
      {/if}
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b border-border-subtle text-left text-xs text-text-muted">
              <th class="py-2 pr-3 font-medium">Resolver</th>
              <th class="py-2 pr-3 font-medium">Endpoint</th>
              <th class="py-2 pr-3 font-medium">Status</th>
              <th class="py-2 pr-3 font-medium">Latency</th>
              <th class="py-2 font-medium"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            {#each resolvers as spec (spec)}
              {@const probe = probeResults[spec]}
              <tr class="border-b border-border-subtle/60 last:border-0">
                <td class="py-2.5 pr-3">
                  <span class="font-mono text-[13px]">{spec}</span>
                  {#if isAlias(spec)}
                    <Badge size="sm" class="ml-2">alias</Badge>
                  {/if}
                </td>
                <td class="max-w-[280px] truncate py-2.5 pr-3 font-mono text-[13px] text-text-muted">
                  {endpointFor(spec)}
                </td>
                <td class="py-2.5 pr-3">
                  {#if probe}
                    <Badge variant={probe.ok ? 'success' : 'error'} size="sm" dot>
                      {probe.ok ? 'Passed' : 'Failed'}
                    </Badge>
                    {#if !probe.ok && probe.error}
                      <p class="mt-1 max-w-[280px] text-xs text-red-400">{probe.error}</p>
                    {/if}
                  {:else}
                    <span class="text-xs text-text-muted">Not tested</span>
                  {/if}
                </td>
                <td class="py-2.5 pr-3">
                  {#if probe?.ok}
                    {probe.latencyMs} ms
                  {:else}
                    <span class="text-text-muted">—</span>
                  {/if}
                </td>
                <td class="py-2.5 text-right">
                  <Button
                    variant="ghost"
                    size="sm"
                    onclick={() => handleRemoveResolver(spec)}
                    disabled={savingUrls || probing}
                    aria-label={`Remove ${spec}`}
                  >
                    <span class="material-symbols-outlined text-[18px]">delete</span>
                  </Button>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <div class="mt-4 flex flex-col gap-2 sm:flex-row sm:items-start">
        <div class="flex-1">
          <Input
            bind:value={newResolver}
            placeholder="cloudflare, google, quad9, adguard, or https://…/dns-query"
            error={resolverError}
          />
          {#if defaults}
            <p class="mt-1 text-xs text-text-muted">
              Known aliases: {Object.keys(defaults.aliases).join(', ')} — or paste any DoH JSON
              endpoint URL.
            </p>
          {/if}
        </div>
        <Button onclick={handleAddResolver} disabled={savingUrls || !newResolver.trim()}>
          Add resolver
        </Button>
      </div>
    </Card>

    <Card title="Provider pinning" subtitle="A pinned provider always sends its traffic through this pool">
      {#if pinnedProviders.length > 0}
        <div class="mb-4 flex flex-wrap items-center gap-2">
          <span class="text-sm text-text-muted">Currently pinned to this pool:</span>
          {#each pinnedProviders as provider (provider)}
            <Badge variant="success">{provider}</Badge>
          {/each}
        </div>
      {:else}
        <p class="mb-4 text-sm text-text-muted">
          No provider is pinned to this pool. Providers without a pin rotate across the active
          pools instead.
        </p>
      {/if}
      <div
        class="flex flex-col gap-3 rounded-lg border border-border/50 p-3 sm:flex-row sm:items-center sm:justify-between"
      >
        <div>
          <p class="font-medium text-sm">opencode</p>
          <p class="text-xs text-text-muted">
            {#if opencodePinned}
              Pinned to {selectedPool.name}
              {#if strategies[OPENCODE_PROVIDER]?.rotateStrategy}
                · rotate: {strategies[OPENCODE_PROVIDER]?.rotateStrategy}
              {/if}
            {:else if strategies[OPENCODE_PROVIDER]?.proxyPoolId}
              Pinned to a different pool
            {:else}
              Not pinned — rotates across active pools
            {/if}
          </p>
        </div>
        {#if opencodePinned}
          <Button variant="outline" size="sm" onclick={() => setOpencodePin(false)} disabled={pinBusy}>
            Lepas pin
          </Button>
        {:else}
          <Button size="sm" onclick={() => setOpencodePin(true)} disabled={pinBusy}>
            Pin ke opencode
          </Button>
        {/if}
      </div>
    </Card>
  </div>
{/if}
