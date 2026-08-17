import net from 'node:net';

export async function createAuthenticatedSocksBridge(rawUpstreamURL) {
  const upstream = parseUpstream(rawUpstreamURL);
  const server = net.createServer((client) => {
    bridgeConnection(client, upstream).catch((error) => client.destroy(error));
  });
  await listen(server);
  const address = server.address();
  return {
    host: '127.0.0.1',
    port: address.port,
    url: `socks5://127.0.0.1:${address.port}`,
    close: () => new Promise((resolve) => server.close(resolve)),
  };
}

async function bridgeConnection(client, upstreamConfig) {
  client.setNoDelay(true);
  const greeting = await readExactly(client, 2);
  if (greeting[0] !== 0x05) throw new Error('unsupported SOCKS version');
  await readExactly(client, greeting[1]);
  client.write(Buffer.from([0x05, 0x00]));

  const requestHead = await readExactly(client, 4);
  if (requestHead[0] !== 0x05 || requestHead[1] !== 0x01) {
    client.write(Buffer.from([0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0]));
    throw new Error('only SOCKS CONNECT is supported');
  }
  const destination = await readDestination(client, requestHead[3]);
  const upstream = net.connect(upstreamConfig.port, upstreamConfig.host);
  upstream.setNoDelay(true);
  try {
    await onceConnected(upstream);
    await authenticateUpstream(upstream, upstreamConfig);
    upstream.write(Buffer.concat([requestHead, destination]));
    const upstreamReply = await readSocksReply(upstream);
    client.write(upstreamReply);
    if (upstreamReply[1] !== 0x00) throw new Error(`upstream SOCKS CONNECT failed with code ${upstreamReply[1]}`);
    client.pipe(upstream);
    upstream.pipe(client);
    const destroyPeer = () => upstream.destroy();
    client.once('close', destroyPeer);
    upstream.once('close', () => client.destroy());
  } catch (error) {
    upstream.destroy();
    throw error;
  }
}

async function authenticateUpstream(socket, config) {
  const hasCredentials = config.username !== '' || config.password !== '';
  socket.write(Buffer.from([0x05, 0x01, hasCredentials ? 0x02 : 0x00]));
  const selection = await readExactly(socket, 2);
  if (selection[0] !== 0x05 || selection[1] === 0xff) throw new Error('upstream SOCKS rejected authentication methods');
  if (selection[1] === 0x00) return;
  if (selection[1] !== 0x02 || !hasCredentials) throw new Error('upstream SOCKS selected unsupported authentication');
  const username = Buffer.from(config.username);
  const password = Buffer.from(config.password);
  if (username.length > 255 || password.length > 255) throw new Error('SOCKS credentials exceed 255 bytes');
  socket.write(Buffer.concat([
    Buffer.from([0x01, username.length]), username,
    Buffer.from([password.length]), password,
  ]));
  const authReply = await readExactly(socket, 2);
  if (authReply[0] !== 0x01 || authReply[1] !== 0x00) throw new Error('upstream SOCKS authentication failed');
}

async function readDestination(socket, addressType) {
  switch (addressType) {
    case 0x01:
      return Buffer.concat([await readExactly(socket, 4), await readExactly(socket, 2)]);
    case 0x03: {
      const length = await readExactly(socket, 1);
      return Buffer.concat([length, await readExactly(socket, length[0]), await readExactly(socket, 2)]);
    }
    case 0x04:
      return Buffer.concat([await readExactly(socket, 16), await readExactly(socket, 2)]);
    default:
      throw new Error(`unsupported SOCKS address type ${addressType}`);
  }
}

async function readSocksReply(socket) {
  const head = await readExactly(socket, 4);
  return Buffer.concat([head, await readDestination(socket, head[3])]);
}

function parseUpstream(raw) {
  const url = new URL(raw);
  if (!['socks5:', 'socks5h:'].includes(url.protocol)) throw new Error(`unsupported upstream proxy scheme ${url.protocol}`);
  if (!url.hostname) throw new Error('upstream SOCKS proxy has no host');
  return {
    host: url.hostname,
    port: Number.parseInt(url.port || '1080', 10),
    username: decodeURIComponent(url.username),
    password: decodeURIComponent(url.password),
  };
}

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
}

function onceConnected(socket) {
  return new Promise((resolve, reject) => {
    const cleanup = () => { socket.off('connect', onConnect); socket.off('error', onError); };
    const onConnect = () => { cleanup(); resolve(); };
    const onError = (error) => { cleanup(); reject(error); };
    socket.once('connect', onConnect);
    socket.once('error', onError);
  });
}

function readExactly(socket, size) {
  if (size === 0) return Promise.resolve(Buffer.alloc(0));
  return readPaused(socket, size);
}

async function readPaused(socket, size) {
  const chunks = [];
  let remaining = size;
  while (remaining > 0) {
    const chunk = socket.read(remaining);
    if (chunk) {
      chunks.push(chunk);
      remaining -= chunk.length;
      continue;
    }
    await waitReadable(socket);
  }
  return Buffer.concat(chunks, size);
}

function waitReadable(socket) {
  return new Promise((resolve, reject) => {
    const cleanup = () => {
      socket.off('readable', onReadable);
      socket.off('error', onError);
      socket.off('close', onClose);
    };
    const onReadable = () => { cleanup(); resolve(); };
    const onError = (error) => { cleanup(); reject(error); };
    const onClose = () => { cleanup(); reject(new Error('socket closed before enough data arrived')); };
    socket.once('readable', onReadable);
    socket.once('error', onError);
    socket.once('close', onClose);
  });
}
