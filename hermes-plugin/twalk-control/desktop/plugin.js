/**
 * Twalk · tour de contrôle (desktop half of the twalk-control package).
 * Read-only pane: collector states and the latest messages collected by Twalk
 * on WhatsApp, Telegram, Signal, Mattermost, Matrix and SMS, with their triage.
 * Data: GET /api/plugins/twalk-control/feed (plugin_api.py), polled every 5 s.
 */
import { cn, useQuery } from '@hermes/plugin-sdk'
import { useState } from 'react'
import { jsx, jsxs } from 'react/jsx-runtime'

const ID = 'twalk-control'

const NETS = {
  whatsapp: 'WhatsApp',
  telegram: 'Telegram',
  signal: 'Signal',
  mattermost: 'Mattermost',
  matrix: 'Matrix',
  sms: 'SMS',
  linkedin: 'LinkedIn'
}

const LABELS = {
  urgent: { text: 'Urgent', color: 'var(--ui-red)' },
  reply: { text: 'À répondre', color: 'var(--ui-orange)' },
  info: { text: 'Info', color: 'var(--ui-blue)' },
  noise: { text: 'Bruit', color: 'var(--ui-text-quaternary)' }
}

const STATES = {
  connected: { text: 'connecté', color: 'var(--ui-green)' },
  disconnected: { text: 'déconnecté', color: 'var(--ui-orange)' },
  logged_out: { text: 'à reconnecter', color: 'var(--ui-red)' },
  stopping: { text: 'arrêté', color: 'var(--ui-text-quaternary)' }
}

function hhmm(iso) {
  const d = new Date(iso)
  const today = new Date()
  const t = d.toLocaleTimeString('fr-FR', { hour: '2-digit', minute: '2-digit' })
  return d.toDateString() === today.toDateString() ? t : `${d.toLocaleDateString('fr-FR', { day: '2-digit', month: '2-digit' })} ${t}`
}

function Dot({ color }) {
  return jsx('span', { className: 'inline-block size-1.5 shrink-0 rounded-full', style: { background: color } })
}

function Chip({ active, onClick, children }) {
  return jsx('button', {
    type: 'button',
    onClick,
    className: cn(
      'rounded px-1.5 py-0.5 text-[0.6875rem] transition-colors',
      active ? 'bg-(--ui-stroke-secondary) text-foreground' : 'text-(--ui-text-tertiary) hover:text-foreground'
    ),
    children
  })
}

function Collectors({ collectors }) {
  const byNet = Object.fromEntries((collectors || []).map(c => [c.network, c]))
  return jsx('div', {
    className: 'flex flex-wrap gap-x-3 gap-y-1',
    children: Object.keys(NETS).map(n => {
      const c = byNet[n]
      const st = c ? STATES[c.state] || STATES.disconnected : { text: 'non branché', color: 'var(--ui-text-quaternary)' }
      return jsxs('span', {
        title: c ? `${NETS[n]} : ${st.text}${c.detail ? ' · ' + c.detail : ''} (${hhmm(c.time)})` : `${NETS[n]} : non branché`,
        className: 'inline-flex items-center gap-1 text-[0.6875rem] text-(--ui-text-secondary)',
        children: [jsx(Dot, { color: st.color }), NETS[n]]
      }, n)
    })
  })
}

function Counters({ labels, filter, setFilter }) {
  return jsx('div', {
    className: 'flex flex-wrap gap-1',
    children: [
      jsx(Chip, { active: filter === 'all', onClick: () => setFilter('all'), children: 'Tout' }, 'all'),
      jsx(Chip, { active: filter === 'prio', onClick: () => setFilter('prio'), children: `À traiter ${(labels?.urgent || 0) + (labels?.reply || 0)}` }, 'prio'),
      ...['urgent', 'reply', 'info', 'noise'].map(l =>
        jsx(Chip, {
          active: filter === l,
          onClick: () => setFilter(l),
          children: jsxs('span', { className: 'inline-flex items-center gap-1', children: [jsx(Dot, { color: LABELS[l].color }), `${LABELS[l].text} ${labels?.[l] || 0}`] })
        }, l)
      )
    ]
  })
}

function Message({ m }) {
  const [open, setOpen] = useState(false)
  const lab = m.label ? LABELS[m.label] : null
  const who = m.sent ? 'Moi' : m.sender
  const where = m.kind === 'dm' ? (m.sent ? `→ ${m.conversation}` : '') : m.conversation
  const text = m.text || (m.attachments?.length ? `[${m.attachments.join(', ')}]` : '')
  return jsxs('button', {
    type: 'button',
    onClick: () => setOpen(!open),
    className: cn(
      'flex w-full flex-col gap-0.5 border-l-2 py-1.5 pl-2 pr-1 text-left transition-colors hover:bg-(--ui-stroke-tertiary)',
      m.sent && 'opacity-60'
    ),
    style: { borderColor: lab ? lab.color : 'var(--ui-stroke-secondary)' },
    children: [
      jsxs('div', {
        className: 'flex items-center gap-1.5 text-[0.6875rem] text-(--ui-text-tertiary)',
        children: [
          jsx('span', { className: 'shrink-0 font-medium text-(--ui-text-secondary)', children: NETS[m.network] || m.network }),
          where && jsx('span', { className: 'truncate', children: `· ${where}` }),
          jsx('span', { className: 'ml-auto shrink-0 tabular-nums', children: hhmm(m.time) })
        ]
      }),
      jsxs('div', {
        className: 'flex items-center gap-1.5 text-xs',
        children: [
          jsx('span', { className: 'truncate font-medium text-foreground', children: who }),
          m.mentions_owner && jsx('span', { className: 'shrink-0 text-[0.625rem] text-(--ui-accent)', children: '@moi' }),
          lab && jsx('span', { className: 'shrink-0 text-[0.625rem]', style: { color: lab.color }, children: lab.text }),
          m.replied && jsx('span', { className: 'shrink-0 text-[0.625rem] text-(--ui-green)', children: '✓ répondu' })
        ]
      }),
      text && jsx('div', { className: cn('text-xs text-(--ui-text-secondary)', !open && 'line-clamp-2'), children: text }),
      open && m.reason && jsx('div', { className: 'text-[0.6875rem] italic text-(--ui-text-tertiary)', children: `Tri : ${m.reason}` })
    ]
  })
}

function makePane(ctx) {
  return function TwalkPane() {
    const [filter, setFilter] = useState('prio')
    const [net, setNet] = useState('all')
    const q = useQuery({
      queryKey: [ID, 'feed', '24h'],
      queryFn: () => ctx.rest('/feed?since=24h&limit=300'),
      refetchInterval: 5000,
      refetchIntervalInBackground: false
    })
    const data = q.data
    if (q.isLoading) return jsx('div', { className: 'p-3 text-xs text-(--ui-text-tertiary)', children: 'Chargement…' })
    if (!data || data.ok === false)
      return jsx('div', {
        className: 'p-3 text-xs text-(--ui-text-tertiary)',
        children: `Bus Twalk injoignable (${data?.error || q.error?.message || 'erreur'}). Le backend du plugin est-il activé ?`
      })
    let msgs = data.messages || []
    if (net !== 'all') msgs = msgs.filter(m => m.network === net)
    if (filter === 'prio') msgs = msgs.filter(m => !m.sent && (m.label === 'urgent' || m.label === 'reply'))
    else if (filter !== 'all') msgs = msgs.filter(m => m.label === filter)
    const nets = Object.keys(data.networks || {})
    return jsxs('div', {
      className: 'flex h-full min-h-0 flex-col',
      children: [
        jsxs('div', {
          className: 'flex flex-col gap-2 border-b border-(--ui-stroke-tertiary) p-2',
          children: [
            jsx(Collectors, { collectors: data.collectors }),
            jsx(Counters, { labels: data.labels, filter, setFilter }),
            nets.length > 1 &&
              jsx('div', {
                className: 'flex flex-wrap gap-1',
                children: ['all', ...nets].map(n =>
                  jsx(Chip, { active: net === n, onClick: () => setNet(n), children: n === 'all' ? 'Tous réseaux' : NETS[n] || n }, n)
                )
              }),
            jsx('div', {
              className: 'text-[0.625rem] text-(--ui-text-quaternary)',
              children: `${data.total} messages sur 24 h · mis à jour ${hhmm(data.generated_at)} · lecture seule`
            })
          ]
        }),
        jsx('div', {
          className: 'min-h-0 flex-1 overflow-y-auto p-1',
          children: msgs.length
            ? msgs.map(m => jsx(Message, { m }, m.id))
            : jsx('div', { className: 'p-3 text-xs text-(--ui-text-tertiary)', children: filter === 'prio' ? 'Rien à traiter. 🎉' : 'Aucun message.' })
        })
      ]
    })
  }
}

export default {
  id: ID,
  name: 'Twalk · tour de contrôle',
  register(ctx) {
    const Pane = makePane(ctx)
    ctx.register({
      id: 'pane',
      area: 'panes',
      title: 'Twalk',
      data: { placement: 'right', width: '340px' },
      render: () => jsx(Pane, {})
    })
  }
}
