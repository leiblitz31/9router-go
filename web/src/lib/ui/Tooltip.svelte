<!--
  Tooltip — the hover bubble this dashboard puts on icon buttons and badges.

  The bubble is `position: fixed` and portalled to `<body>` while it is open:
  `position: absolute` inside the trigger's wrapper is clipped by any ancestor
  with `overflow` set, and these triggers sit inside `overflow-y-auto`
  connection lists and `overflow-x-auto` tables — the same clipping class as
  the dropdown panels `portal` already fixes.

  The bubble centers above the trigger and flips below it when the space
  above is too short (`tooltipPosition`). The panel is fixed + portalled, so
  it no longer moves with the page: it closes on scroll/resize rather than
  chasing the trigger.
-->
<script lang="ts">
  import { tick } from 'svelte'
  import type { Snippet } from 'svelte'
  import { portal } from './portal'
  import { placeTooltip } from './tooltipPosition'

  interface Props {
    /** Bubble text; an empty string never opens. */
    text: string
    class?: string
    children?: Snippet
  }

  let { text, class: klass = '', children }: Props = $props()

  let trigger = $state<HTMLElement | null>(null)
  let tip = $state<HTMLElement | null>(null)
  let open = $state(false)
  // Hidden until measured: the bubble cannot be placed before it renders.
  let style = $state('visibility:hidden')

  async function show(): Promise<void> {
    if (!text || !trigger || open) return
    open = true
    await tick()
    const r = trigger.getBoundingClientRect()
    const t = tip?.getBoundingClientRect()
    if (!t || t.width === 0) {
      open = false
      return
    }
    const p = placeTooltip({ rect: r, tip: { width: t.width, height: t.height } })
    style = `left:${Math.round(p.left)}px;top:${Math.round(p.top)}px`
  }

  function hide(): void {
    open = false
    style = 'visibility:hidden'
  }

  $effect(() => {
    if (!open) return
    window.addEventListener('resize', hide)
    document.addEventListener('scroll', hide, true)
    return () => {
      window.removeEventListener('resize', hide)
      document.removeEventListener('scroll', hide, true)
    }
  })
</script>

<span
  bind:this={trigger}
  role="group"
  onmouseenter={show}
  onmouseleave={hide}
  onfocusin={show}
  onfocusout={hide}
  class="inline-flex {klass}"
>
  {@render children?.()}
</span>

{#if open}
  <div
    bind:this={tip}
    use:portal
    role="tooltip"
    {style}
    class="pointer-events-none fixed z-[100] w-max max-w-56 rounded bg-gray-900 px-2 py-1 text-[11px] leading-snug whitespace-normal text-white shadow-lg"
  >
    {text}
  </div>
{/if}
