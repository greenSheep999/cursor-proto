export async function browserFetchAndStream({
  url,
  headers,
  bodyBase64,
  responseStartTimeoutMs,
}) {
  const raw = atob(bodyBase64);
  const body = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i += 1) body[i] = raw.charCodeAt(i);

  const controller = new AbortController();
  const responseStartTimer = setTimeout(() => {
    controller.abort(new Error(`response start timeout after ${responseStartTimeoutMs}ms`));
  }, responseStartTimeoutMs);

  let responseStarted = false;
  const markResponseStarted = () => {
    if (responseStarted) return;
    responseStarted = true;
    clearTimeout(responseStartTimer);
  };

  try {
    const upstream = await fetch(url, {
      method: 'POST',
      headers,
      body,
      credentials: 'omit',
      cache: 'no-store',
      signal: controller.signal,
    });
    await globalThis.__cursorSidecarStart(
      upstream.status,
      Object.fromEntries(upstream.headers.entries()),
    );
    const reader = upstream.body?.getReader();
    if (reader) {
      while (true) {
        const { done, value } = await reader.read();
        if (done) {
          markResponseStarted();
          break;
        }
        if (value.length > 0) markResponseStarted();
        let binary = '';
        const stride = 0x8000;
        for (let i = 0; i < value.length; i += stride) {
          binary += String.fromCharCode(...value.subarray(i, i + stride));
        }
        await globalThis.__cursorSidecarChunk(btoa(binary));
      }
    } else {
      markResponseStarted();
    }
    await globalThis.__cursorSidecarEnd();
  } catch (error) {
    if (controller.signal.aborted) {
      throw new Error(`Chromium response start timeout after ${responseStartTimeoutMs}ms`);
    }
    throw error;
  } finally {
    clearTimeout(responseStartTimer);
  }
}
