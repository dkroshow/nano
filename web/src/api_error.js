// readAPIError keeps client error handling aligned with Nano's JSON envelope,
// while retaining readable failures from a proxy or older server. envelope
// says the body was Nano's own {error: {code, message}}, which every error
// Nano writes carries; a front answering for Nano (Tailscale Serve, a dev
// proxy) sends HTML, text, other JSON or nothing.
export async function readAPIError(response, fallback = `request failed (${response?.status ?? 'unknown'})`) {
  let raw = ''
  try {
    raw = (await response.text()).trim()
  } catch {
    return { code: '', message: fallback, details: null, envelope: false }
  }
  if (!raw) return { code: '', message: fallback, details: null, envelope: false }
  try {
    const body = JSON.parse(raw)
    const error = body?.error
    return {
      envelope: typeof error?.code === 'string' && error.code !== '',
      code: typeof error?.code === 'string' ? error.code : '',
      message: typeof error?.message === 'string' && error.message
        ? error.message
        : typeof error === 'string' && error
          ? error
          : typeof body?.reason === 'string' && body.reason
            ? body.reason
            : fallback,
      details: error?.details ?? null,
    }
  } catch {
    return { code: '', message: raw, details: null, envelope: false }
  }
}
