<script lang="ts">
  // Petani Proxy - dedicated page for the embedded rotating proxy gateway.
  // The gateway is managed by 9Router (start/stop) and used as a native
  // proxy option for providers, without needing a proxy pool entry.
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
  }

  let status = $state<GatewayStatus | null>(null)
  let loading = $state(true)
  let toggling = $state(false)

  async function fetchStatus() {
    try {
      status = await api.getProxyGatewayStatus()
    } catch (e) {
      console.error('Failed to fetch gateway status:', e)
    } finally {
      loading = false
    }
  }

  async function handleToggle(enabled: boolean) {
    if (!status?.available) {
      notifications.error('Petani-proxy tidak terinstall')
      return
    }
    toggling = true
    try {
      status = await api.toggleProxyGateway(enabled)
      notifications.success(enabled ? 'Gateway dinyalakan' : 'Gateway dimatikan')
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
    } catch (e) {
      notifications.error('Gagal restart gateway')
    } finally {
      toggling = false
    }
  }

  onMount(() => {
    fetchStatus()
    const interval = setInterval(fetchStatus, 10000)
    return () => clearInterval(interval)
  })
</script>

<div class="mx-auto flex w-full max-w-5xl flex-col gap-4 px-1 sm:gap-6 sm:px-0">
  <div class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
    <div class="min-w-0">
      <h1 class="text-xl font-semibold sm:text-2xl">Petani Proxy</h1>
      <p class="text-sm text-muted">Embedded rotating proxy gateway — managed by 9Router</p>
    </div>
    {#if status}
      <Badge variant={status.running ? 'success' : 'secondary'}>
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
            <div class="text-muted">PID</div>
            <div class="font-medium">{status.pid || '-'}</div>
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

        {#if !status.available}
          <div class="rounded-lg bg-warning/10 p-3 text-sm text-warning">
            ⚠️ Petani-proxy tidak terinstall di ~/vps-persist/petani-proxy/
          </div>
        {/if}

        <div class="flex items-center justify-between border-t border-border-subtle pt-4">
          <div class="flex items-center gap-3">
            <Toggle
              checked={status.enabled}
              disabled={toggling || !status.available}
              onchange={(e) => handleToggle(e.currentTarget.checked)}
            />
            <span class="text-sm font-medium">
              {status.enabled ? 'Gateway ON' : 'Gateway OFF'}
            </span>
          </div>
          {#if status.running}
            <Button variant="outline" size="sm" disabled={toggling} onclick={handleRestart}>
              Restart
            </Button>
          {/if}
        </div>
      </div>
    </Card>

    <!-- How to use card -->
    <Card>
      <div class="flex flex-col gap-3 p-4">
        <h3 class="font-semibold">Cara Pakai</h3>
        <ol class="list-decimal list-inside text-sm text-muted space-y-1">
          <li>Nyalakan gateway dengan toggle di atas</li>
          <li>
            Buka <span class="font-medium text-foreground">Providers</span> → pilih provider
            (mis. OpenCode) → <span class="font-medium text-foreground">Proxy</span> →
            pilih <span class="font-medium text-foreground">Petani Proxy</span>
            (opsi native, tepat di bawah "None (direct)")
          </li>
          <li>Request akan lewat gateway yang auto-rotate IP tiap request</li>
          <li>Kalau IP kena flag/rate-limit, matikan-nyalakan gateway untuk IP fresh</li>
        </ol>
        <p class="text-xs text-muted">
          Tidak perlu buat proxy pool entry — Petani Proxy adalah opsi bawaan 9Router.
        </p>
      </div>
    </Card>
  {/if}
</div>
