<script lang="ts">
  // Petani Proxy - dedicated page for the embedded rotating proxy gateway.
  // Go-native gateway managed by 9Router with live proxy pool.
  import { onMount } from 'svelte'
  import Card from '../lib/ui/Card.svelte'
  import Button from '../lib/ui/Button.svelte'
  import Toggle from '../lib/ui/Toggle.svelte'
  import Badge from '../lib/ui/Badge.svelte'
  import { api } from '../api/client'
  import { notifications } from '../lib/notifications'

  interface GatewayStatus {
    running: boolean
    enabled: boolean
    available: boolean
    pid?: number
    uptime?: string
    startedAt?: string
    gatewayUrl: string
    proxyCount?: number
  }

  interface PoolProxy {
    IP: string
    Port: number
    Protocol: string
    Country: string
    CountryCode: string
    Anonymity: string
  }

  interface PoolStats {
    PoolSize: number
    TotalRouted: number
    Successful: number
    Failed: number
    SuccessRatePercent: number
    ActiveProxy: string
  }

  let status = $state<GatewayStatus | null>(null)
  let proxies = $state<PoolProxy[]>([])
  let poolStats = $state<PoolStats | null>(null)
  let loading = $state(true)
  let toggling = $state(false)
  let harvesting = $state(false)
  let workers = $state(5)

  async function fetchStatus() {
    try {
      status = await api.getProxyGatewayStatus()
    } catch (e) {
      console.error('Failed to fetch gateway status:', e)
    } finally {
      loading = false
    }
  }

  async function fetchPool() {
    try {
      const pool = await api.getProxyGatewayPool()
      proxies = pool.proxies || []
      poolStats = pool.stats || null
    } catch (e) {
      // Pool not available when gateway is down — not an error
      proxies = []
      poolStats = null
    }
  }

  async function refresh() {
    await fetchStatus()
    await fetchPool()
  }

  async function handleToggle(enabled: boolean) {
    toggling = true
    try {
      status = await api.toggleProxyGateway(enabled)
      notifications.success(enabled ? 'Gateway dinyalakan' : 'Gateway dimatikan')
      await fetchPool()
    } catch (e) {
      notifications.error('Gagal mengubah status gateway')
    } finally {
      toggling = false
    }
  }

  async function handleRestart() {
    toggling = true
    try {
      status = await api.restartProxyGateway()
      notifications.success('Gateway di-restart')
      await fetchPool()
    } catch (e) {
      notifications.error('Gagal restart gateway')
    } finally {
      toggling = false
    }
  }

  async function handleHarvest() {
    harvesting = true
    try {
      const res = await api.harvestProxyGateway(workers)
      notifications.success(`Harvest selesai: ${res.proxyCount} proxy hidup`)
      await fetchPool()
      await fetchStatus()
    } catch (e) {
      notifications.error('Harvest gagal — gateway mungkin mati')
    } finally {
      harvesting = false
    }
  }

  onMount(() => {
    refresh()
    const interval = setInterval(refresh, 10000)
    return () => clearInterval(interval)
  })
</script>

<div class="mx-auto flex w-full max-w-5xl flex-col gap-4 px-1 sm:gap-6 sm:px-0">
  <div class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
    <div class="min-w-0">
      <h1 class="text-xl font-semibold sm:text-2xl">Petani Proxy</h1>
      <p class="text-sm text-muted">Go-native rotating proxy gateway — embedded di 9Router</p>
    </div>
    {#if status}
      <Badge variant={status.running ? 'success' : 'neutral'}>
        {status.running ? 'Running' : 'Stopped'}
      </Badge>
    {/if}
  </div>

  {#if loading}
    <div class="h-32 animate-pulse rounded-[14px] bg-surface border border-border-subtle"></div>
  {:else if status}
    <!-- Main control card -->
    <Card>
      <div class="flex flex-col gap-4 p-4">
        <div class="flex items-center gap-3">
          <div class="flex h-12 w-12 items-center justify-center rounded-lg bg-primary/10">
            <span class="text-2xl">🌐</span>
          </div>
          <div>
            <h3 class="font-semibold">Gateway Status</h3>
            <p class="font-mono text-xs text-muted">{status.gatewayUrl}</p>
          </div>
        </div>

        <div class="grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
          <div>
            <div class="text-muted">Status</div>
            <div class="font-medium">{status.running ? '🟢 Aktif' : '⚪ Mati'}</div>
          </div>
          <div>
            <div class="text-muted">Proxy Hidup</div>
            <div class="font-medium">{status.proxyCount ?? proxies.length}</div>
          </div>
          <div>
            <div class="text-muted">Uptime</div>
            <div class="font-medium">{status.uptime || '-'}</div>
          </div>
          <div>
            <div class="text-muted">Mulai</div>
            <div class="font-medium text-xs">{status.startedAt ? new Date(status.startedAt).toLocaleString() : '-'}</div>
          </div>
        </div>

        <div class="flex items-center justify-between border-t border-border-subtle pt-4">
          <div class="flex items-center gap-3">
            <Toggle
              checked={status.enabled}
              disabled={toggling}
              onchange={(v) => handleToggle(v)}
            />
            <span class="text-sm font-medium">
              {status.enabled ? 'Gateway ON' : 'Gateway OFF'}
            </span>
          </div>
          <div class="flex gap-2 items-center">
            {#if status.running}
              <select
                bind:value={workers}
                disabled={harvesting}
                class="bg-muted border border-border rounded-md px-2 py-1 text-sm"
                title="Jumlah koneksi paralel saat fetch (lebih tinggi = lebih cepat tapi rawan rate-limit)"
              >
                <option value={3}>3 par</option>
                <option value={5}>5 par</option>
                <option value={10}>10 par</option>
                <option value={15}>15 par</option>
                <option value={30}>30 par</option>
              </select>
              <Button variant="outline" size="sm" disabled={toggling || harvesting} onclick={handleHarvest}>
                {harvesting ? '⏳ Harvesting...' : '🌾 Harvest Proxy'}
              </Button>
              <Button variant="outline" size="sm" disabled={toggling} onclick={handleRestart}>
                Restart
              </Button>
            {/if}
          </div>
        </div>
      </div>
    </Card>

    <!-- Pool stats -->
    {#if poolStats && status.running}
      <Card>
        <div class="flex flex-col gap-3 p-4">
          <h3 class="font-semibold">📊 Statistik Pool</h3>
          <div class="grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
            <div>
              <div class="text-muted">Total Di-route</div>
              <div class="font-medium text-lg">{poolStats.TotalRouted}</div>
            </div>
            <div>
              <div class="text-muted">Sukses</div>
              <div class="font-medium text-lg text-green-500">{poolStats.Successful}</div>
            </div>
            <div>
              <div class="text-muted">Gagal</div>
              <div class="font-medium text-lg text-red-500">{poolStats.Failed}</div>
            </div>
            <div>
              <div class="text-muted">Success Rate</div>
              <div class="font-medium text-lg">{poolStats.SuccessRatePercent.toFixed(1)}%</div>
            </div>
          </div>
          {#if poolStats.ActiveProxy}
            <div class="text-xs text-muted">
              Proxy aktif: <span class="font-mono text-foreground">{poolStats.ActiveProxy}</span>
            </div>
          {/if}
        </div>
      </Card>
    {/if}

    <!-- Proxy list -->
    {#if status.running}
      <Card>
        <div class="flex flex-col gap-3 p-4">
          <div class="flex items-center justify-between">
            <h3 class="font-semibold">🔗 Proxy Hidup ({proxies.length})</h3>
            <span class="text-xs text-muted">Auto-refresh 10 dtk</span>
          </div>
          {#if proxies.length === 0}
            <div class="rounded-lg bg-surface-2 p-4 text-center text-sm text-muted">
              Pool kosong. Klik <span class="font-medium">🌾 Harvest Proxy</span> untuk mencari proxy baru,
              atau tunggu harvest otomatis (tiap 10 menit).
            </div>
          {:else}
            <div class="max-h-96 overflow-y-auto rounded-lg border border-border-subtle">
              <table class="w-full text-sm">
                <thead class="sticky top-0 bg-surface-2">
                  <tr class="text-left text-xs text-muted">
                    <th class="px-3 py-2">Proxy</th>
                    <th class="px-3 py-2">Negara</th>
                    <th class="px-3 py-2">Anonimitas</th>
                    <th class="px-3 py-2">Protokol</th>
                  </tr>
                </thead>
                <tbody>
                  {#each proxies.slice(0, 50) as p}
                    <tr class="border-t border-border-subtle hover:bg-surface-2/50">
                      <td class="px-3 py-2 font-mono text-xs">{p.IP}:{p.Port}</td>
                      <td class="px-3 py-2">{p.Country || '-'}</td>
                      <td class="px-3 py-2">
                        {#if p.Anonymity === 'Elite'}
                          <span class="text-green-500">🛡️ Elite</span>
                        {:else if p.Anonymity === 'Anonymous'}
                          <span class="text-yellow-500">🔒 Anonymous</span>
                        {:else}
                          <span class="text-muted">{p.Anonymity || '-'}</span>
                        {/if}
                      </td>
                      <td class="px-3 py-2 text-xs">{p.Protocol || 'http'}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>
              {#if proxies.length > 50}
                <div class="p-2 text-center text-xs text-muted">
                  ... dan {proxies.length - 50} lainnya
                </div>
              {/if}
            </div>
          {/if}
        </div>
      </Card>
    {/if}

    <!-- How to use card -->
    <Card>
      <div class="flex flex-col gap-3 p-4">
        <h3 class="font-semibold">Cara Pakai</h3>
        <ol class="list-decimal list-inside text-sm text-muted space-y-1">
          <li>Nyalakan gateway dengan toggle di atas</li>
          <li>Klik <span class="font-medium text-foreground">🌾 Harvest Proxy</span> untuk isi pool (atau tunggu otomatis)</li>
          <li>
            Buka <span class="font-medium text-foreground">Providers</span> → pilih provider
            (mis. OpenCode) → <span class="font-medium text-foreground">Proxy</span> →
            pilih <span class="font-medium text-foreground">Petani Proxy</span>
            (opsi native, tepat di bawah "None (direct)")
          </li>
          <li>Request akan lewat gateway yang auto-rotate IP tiap request</li>
        </ol>
      </div>
    </Card>
  {/if}
</div>
