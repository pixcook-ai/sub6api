import { describe, expect, it } from 'vitest'
import { authorizePelicanInlineScripts, extractPelicanHtml, readPelicanPreviewNonce } from '../pelicanHtml'

describe('Pelican preview script authorization', () => {
  it('reads a valid parent nonce and rejects invalid values', () => {
    const doc = document.implementation.createHTMLDocument()
    const script = doc.createElement('script')
    script.nonce = 'parent-nonce+123/=='
    doc.head.appendChild(script)
    expect(readPelicanPreviewNonce(doc)).toBe('parent-nonce+123/==')
    script.nonce = 'invalid nonce'
    expect(readPelicanPreviewNonce(doc)).toBe('')
    script.remove()
    expect(readPelicanPreviewNonce(doc)).toBe('')
  })

  it('authorizes only inline scripts and preserves the restrictive preview policy', () => {
    const raw = '<html><body><script nonce="old">window.animate = true</script><script src="/external.js" nonce="old"></script><svg><script href="/external.svg" nonce="old"></script><script xlink:href="/legacy.svg" nonce="old"></script><script>window.svgAnimate = true</script></svg></body></html>'
    const extracted = extractPelicanHtml(raw)
    const preview = new DOMParser().parseFromString(authorizePelicanInlineScripts(extracted, 'current-nonce'), 'text/html')
    const scripts = [...preview.querySelectorAll('script')]
    expect(scripts.map(script => script.getAttribute('nonce'))).toEqual(['current-nonce', null, null, null, 'current-nonce'])
    expect(preview.querySelector('meta[http-equiv="Content-Security-Policy"]')?.getAttribute('content')).toContain("connect-src 'none'")
    expect(extracted).not.toContain('current-nonce')
    expect(raw).toContain('nonce="old"')
  })

  it('removes stale nonces when there is no valid parent nonce', () => {
    const html = '<html><body><script nonce="old">window.animate = true</script></body></html>'
    for (const nonce of ['', 'invalid nonce']) {
      const preview = new DOMParser().parseFromString(authorizePelicanInlineScripts(html, nonce), 'text/html')
      expect(preview.querySelector('script')?.hasAttribute('nonce')).toBe(false)
    }
  })
})
