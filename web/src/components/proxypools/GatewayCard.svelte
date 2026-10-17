<script lang="ts">
  // Embedded Proxy Gateway Card (petani-proxy)
  // Shows gateway status with on/off toggle, integrated with 9Router proxy pools.
  import { onMount } from 'svelte'
  import Card from '../../lib/ui/Card.svelte'
  import Button from '../../lib/ui/Button.svelte'
  import Toggle from '../../lib/ui/Toggle.svelte'
  import Badge from '../../lib/ui/Badge.svelte'
  import { api } from '../../api/client'
  import { notifications } from '../../lib/notifications'

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
      notifications.success(
        enabled ? 'Gateway dinyalakan' : 'Gateway dimatikan'
      )
    } catch (e) {
      notifications.error('Gagal mengubah status gateway')
      console.error(e)
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
      console.error(e)
    } finally {
      toggling = false
    }
  }

  onMount(() => {
    fetchStatus()
    // Refresh every 10s
    const interval = setInterval(fetchStatus, 10000)
    return () => clearInterval(interval)
  })
</script>

<Card>
  <div class="flex flex-col gap-4 p-4">
    <!-- Header -->
    <div class="flex items-center justify-between">
      <div class="flex items-center gap-3">
        <div class="flex h-10 w-10 items-center justify-center rounded-lg bg-primary/10">
          <span class="text-xl">🌐</span>
        </div>
        <div>
          <h3 class="font-semibold">Petani-Proxy Gateway</h3>
          <p class="text-sm text-muted">Local rotating proxy (127.0.0.1:8888)</p>
        </div>
      </div>
      {#if status}
        <Badge variant={status.running ? 'success' : 'secondary'}>
          {status.running ? 'Running' : 'Stopped'}
        </Badge>
      {/if}
    </div>

    <!-- Status details -->
    {#if loading}
      <div class="h-16 animate-pulse rounded bg-surface"></div>
    {:else if status}
      <div class="grid grid-cols-2 gap-3 text-sm sm:grid-cols-4">
        <div>
          <div class="text-muted">Status</div>
          <div class="font-medium">
            {status.running ? '🟢 Aktif' : '⚪ Mati'}
          </div>
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
          <div class="text-muted">URL</div>
          <div class="font-mono text-xs">{status.gatewayUrl}</div>
        </div>
      </div>

      {#if !status.available}
        <div class="rounded-lg bg-warning/10 p-3 text-sm text-warning">
          ⚠️ Petani-proxy tidak terinstall di ~/vps-persist/petani-proxy/
        </div>
      {/if}

      <!-- Controls -->
      <div class="flex items-center justify-between border-t border-border-subtle pt-4">
        <div class="flex items-center gap-3">
          <Toggle
            checked={status.enabled}
            disabled={toggling || !status.available}
            onchange={(e) => handleToggle(e.currentTarget.checked)}
          />
          <span class="text-sm">
            {status.enabled ? 'Gateway ON' : 'Gateway OFF'}
          </span>
        </div>
        {#if status.running}
          <Button
            variant="outline"
            size="sm"
            disabled={toggling}
            onclick={handleRestart}
          >
            Restart
          </Button>
        {/if}
      </div>

      <p class="text-xs text-muted">
        Ketika ON, proxy pool "petani-proxy-local" aktif dan request oc/ akan
        lewat gateway yang auto-rotate IP. Ketika OFF, pool di-nonaktifkan.
      </p>
    {/if}
  </div>
</Card>
