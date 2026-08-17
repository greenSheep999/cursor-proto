import assert from 'node:assert/strict';
import net from 'node:net';
import test from 'node:test';

import { createAuthenticatedSocksBridge } from '../src/socks_bridge.mjs';

test('bridges unauthenticated SOCKS5 clients through an authenticated upstream', async (t) => {
  const credentials = { username: 'bridge-user', password: 'bridge-pass' };
  const upstreamSockets = new Set();
  const upstream = net.createServer((socket) => {
    upstreamSockets.add(socket);
    socket.once('close', () => upstreamSockets.delete(socket));
    handleAuthenticatedUpstream(socket, credentials);
  });
  await listen(upstream);

  const upstreamAddress = upstream.address();
  const bridge = await createAuthenticatedSocksBridge(
    `socks5://${credentials.username}:${credentials.password}@127.0.0.1:${upstreamAddress.port}`,
  );

  const socket = net.connect(bridge.port, bridge.host);
  t.after(async () => {
    socket.destroy();
    for (const upstreamSocket of upstreamSockets) upstreamSocket.destroy();
    await bridge.close();
    await close(upstream);
  });
  await onceConnected(socket);

  socket.write(Buffer.from([0x05, 0x01, 0x00]));
  assert.deepEqual(await readExactly(socket, 2), Buffer.from([0x05, 0x00]));

  const host = Buffer.from('example.test');
  socket.write(Buffer.concat([
    Buffer.from([0x05, 0x01, 0x00, 0x03, host.length]),
    host,
    Buffer.from([0x01, 0xbb]),
  ]));
  const reply = await readExactly(socket, 10);
  assert.equal(reply[0], 0x05);
  assert.equal(reply[1], 0x00);

  socket.write('bridge-ok');
  assert.equal((await readExactly(socket, 9)).toString(), 'bridge-ok');
});

async function handleAuthenticatedUpstream(socket, credentials) {
  try {
    assert.deepEqual(await readExactly(socket, 3), Buffer.from([0x05, 0x01, 0x02]));
    socket.write(Buffer.from([0x05, 0x02]));

    const authHead = await readExactly(socket, 2);
    const username = (await readExactly(socket, authHead[1])).toString();
    const passwordLength = (await readExactly(socket, 1))[0];
    const password = (await readExactly(socket, passwordLength)).toString();
    assert.equal(username, credentials.username);
    assert.equal(password, credentials.password);
    socket.write(Buffer.from([0x01, 0x00]));

    const requestHead = await readExactly(socket, 5);
    assert.deepEqual(requestHead.subarray(0, 4), Buffer.from([0x05, 0x01, 0x00, 0x03]));
    const requestedHost = (await readExactly(socket, requestHead[4])).toString();
    const requestedPort = (await readExactly(socket, 2)).readUInt16BE();
    assert.equal(requestedHost, 'example.test');
    assert.equal(requestedPort, 443);
    socket.write(Buffer.from([0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0x23, 0x45]));
    socket.on('data', (chunk) => socket.write(chunk));
  } catch (error) {
    socket.destroy(error);
  }
}

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
}

function close(server) {
  return new Promise((resolve) => server.close(resolve));
}

function onceConnected(socket) {
  return new Promise((resolve, reject) => {
    socket.once('connect', resolve);
    socket.once('error', reject);
  });
}

function readExactly(socket, size) {
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
    await new Promise((resolve, reject) => {
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
  return Buffer.concat(chunks, size);
}
