import type { ProxyNode } from '@/api/proxy'

const DEFAULT_PROXY_NODE_IMAGE_CONCURRENCY = 30
const MAX_MANUAL_PROXY_NODE_IMPORTS = 5000
const MAX_MANUAL_PROXY_NODE_INPUT_LENGTH = 512 * 1024
const SUPPORTED_PROXY_SCHEMES = new Set(['http:', 'https:', 'socks4:', 'socks5:'])
const DEFAULT_PROXY_PORTS: Record<string, string> = {
  'http:': '80',
  'https:': '443',
  'socks4:': '1080',
  'socks5:': '1080',
}

export interface ProxyNodeImportResult {
  nodes: ProxyNode[]
  duplicateCount: number
  invalidCount: number
  inputCount: number
  truncatedCount: number
}

function createGeneratedId() {
  let suffix = ''
  try {
    suffix = globalThis.crypto?.randomUUID?.().replace(/-/g, '').slice(0, 12) || ''
  } catch {
    suffix = ''
  }
  if (!suffix) {
    suffix = `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`.slice(0, 12)
  }
  return `node-${suffix}`
}

function splitInput(value: string) {
  const delimiter = /[,;](?=(?:(?:https?|socks4|socks5h?):\/\/|(?:[^,\s]+@)?(?:[^:@,\s]+|\[[^\]]+\]):\d+))/i
  const tokens: string[] = []
  for (const line of String(value || '').replace(/\r\n?/g, '\n').split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#') || trimmed.startsWith('//')) continue
    for (const chunk of trimmed.split(/\s+/)) {
      for (const item of chunk.split(delimiter)) {
        const token = item.trim().replace(/^['"]|['"]$/g, '')
        if (token && !token.startsWith('#') && !token.startsWith('//')) tokens.push(token)
      }
    }
  }
  return tokens
}

function normalizeProxyLink(value: string) {
  let raw = value.trim().replace(/^['"]|['"]$/g, '')
  if (!raw || raw.startsWith('#') || raw.startsWith(';') || raw.startsWith('//')) return ''

  if (raw.toLowerCase().startsWith('socks5h://')) {
    raw = `socks5://${raw.slice('socks5h://'.length)}`
  } else if (!raw.includes('://')) {
    const parts = raw.split(':')
    if (parts.length === 4 && /^\d+$/.test(parts[1] || '')) {
      raw = `http://${parts[2]}:${parts[3]}@${parts[0]}:${parts[1]}`
    } else {
      raw = `http://${raw}`
    }
  }

  let parsed: URL
  try {
    parsed = new URL(raw)
  } catch {
    return ''
  }
  if (!SUPPORTED_PROXY_SCHEMES.has(parsed.protocol) || !parsed.hostname) return ''
  const authority = raw.slice(raw.indexOf('://') + 3).split(/[/?#]/, 1)[0] || ''
  const hostPort = authority.slice(authority.lastIndexOf('@') + 1)
  const hasExplicitPort = hostPort.startsWith('[') ? /]:\d+$/.test(hostPort) : /:\d+$/.test(hostPort)
  if (!hasExplicitPort) return ''
  const port = Number(parsed.port || DEFAULT_PROXY_PORTS[parsed.protocol])
  if (!Number.isInteger(port) || port < 1 || port > 65535) return ''

  // Keep the original URL shape so credentials and URL encoding are not rewritten.
  return raw
}

function proxyLinkKey(value: string) {
  try {
    const parsed = new URL(value)
    return [
      parsed.protocol.toLowerCase(),
      parsed.username,
      parsed.password,
      parsed.hostname.toLowerCase(),
      parsed.port || DEFAULT_PROXY_PORTS[parsed.protocol] || '',
      parsed.pathname,
      parsed.search,
    ].join('|')
  } catch {
    return value
  }
}

function nodeName(value: string, index: number) {
  try {
    const parsed = new URL(value)
    const host = parsed.hostname.replace(/^\[|\]$/g, '')
    return `${host}:${parsed.port || DEFAULT_PROXY_PORTS[parsed.protocol] || '-'}`
  } catch {
    return `手工节点 ${index + 1}`
  }
}

export function parseProxyNodeLinks(
  input: string,
  existingUrls: Iterable<string> = [],
  imageConcurrencyLimit = DEFAULT_PROXY_NODE_IMAGE_CONCURRENCY,
): ProxyNodeImportResult {
  const boundedInput = String(input || '').slice(0, MAX_MANUAL_PROXY_NODE_INPUT_LENGTH)
  const allTokens = splitInput(boundedInput)
  const tokens = allTokens.slice(0, MAX_MANUAL_PROXY_NODE_IMPORTS)
  const truncatedCount = Math.max(
    0,
    allTokens.length - tokens.length,
  ) + (String(input || '').length > MAX_MANUAL_PROXY_NODE_INPUT_LENGTH ? 1 : 0)
  const seen = new Set<string>()
  for (const existingUrl of existingUrls) {
    const normalized = normalizeProxyLink(String(existingUrl || ''))
    if (normalized) seen.add(proxyLinkKey(normalized))
  }

  const nodes: ProxyNode[] = []
  let duplicateCount = 0
  let invalidCount = 0
  for (const token of tokens) {
    const normalized = normalizeProxyLink(token)
    if (!normalized) {
      invalidCount += 1
      continue
    }
    const key = proxyLinkKey(normalized)
    if (seen.has(key)) {
      duplicateCount += 1
      continue
    }
    seen.add(key)
    nodes.push({
      id: createGeneratedId(),
      name: nodeName(normalized, nodes.length),
      url: normalized,
      enabled: true,
      image_concurrency_limit: Math.max(0, Math.min(10000, Math.floor(Number(imageConcurrencyLimit) || 0))),
      notes: '',
      source: 'manual',
      subscription_managed: false,
    })
  }

  return {
    nodes,
    duplicateCount,
    invalidCount,
    inputCount: allTokens.length,
    truncatedCount,
  }
}
