import assert from "node:assert/strict";
import test from "node:test";
import childProcess from "node:child_process";
import { EventEmitter } from "node:events";
import { syncBuiltinESMExports } from "node:module";
import { PassThrough } from "node:stream";
import { rotateProxySession, TlsFingerprintTransport } from "./tls-transport.mjs";

test("proxy username rotation preserves literal percent escapes and the password", () => {
  const proxy = new URL("http://proxy.invalid:8080");
  proxy.username = encodeURIComponent("user%2F%40%25-sid-FIXED-country-US");
  proxy.password = encodeURIComponent("password%2F@:/?#");

  const rotated = new URL(rotateProxySession(proxy.toString()));

  assert.match(decodeURIComponent(rotated.username), /^user%2F%40%25-sid-[A-Za-z0-9]{8}-country-US$/);
  assert.equal(rotated.password, proxy.password);
  assert.equal(rotated.origin, proxy.origin);
});

test("proxy password rotation preserves literal percent escapes and the username", () => {
  const proxy = new URL("socks5h://proxy.invalid:1080");
  proxy.username = encodeURIComponent("user%2F@:/?#");
  proxy.password = encodeURIComponent("password%2F%40%25-US-12345678-30m");

  const rotated = new URL(rotateProxySession(proxy.toString()));

  assert.match(decodeURIComponent(rotated.password), /^password%2F%40%25-US-[0-9]{8}-30m$/);
  assert.equal(rotated.username, proxy.username);
  assert.equal(rotated.origin, proxy.origin);
});

test("proxy rotation keeps credentials without a session marker unchanged", () => {
  const proxy = "https://user%252F:password%2540@proxy.invalid:8080/";

  assert.equal(rotateProxySession(proxy), proxy);
});

test("first proxy preparation rotates its session and preserves the remaining credentials", async () => {
  const proxy = new URL("http://proxy.invalid:8080");
  proxy.username = encodeURIComponent("user%2F-sid-FIXED-country-US");
  proxy.password = encodeURIComponent("password%40");
  const transport = new TlsFingerprintTransport();
  let configuredProxy;
  transport.configure = async (value) => {
    configuredProxy = value;
  };
  transport.request = async () => ({ status: 200, headers: new Headers() });

  const selectedProxy = await transport.prepareProxy(proxy.toString(), "https://chatgpt.com/");
  const selected = new URL(selectedProxy);

  assert.equal(selectedProxy, configuredProxy);
  assert.notEqual(selected.username, proxy.username);
  assert.match(decodeURIComponent(selected.username), /^user%2F-sid-[A-Za-z0-9]{8}-country-US$/);
  assert.equal(selected.password, proxy.password);
  assert.equal(selected.origin, proxy.origin);
});

function mockTlsWorkers(context) {
  const workers = [];
  const messages = [];
  context.mock.method(childProcess, "spawnSync", () => ({ status: 0 }));
  context.mock.method(childProcess, "spawn", () => {
    const worker = new EventEmitter();
    worker.stdout = new PassThrough();
    worker.stderr = new PassThrough();
    worker.respond = true;
    worker.kill = () => {
      worker.emit("close", 0, "SIGTERM");
      return true;
    };
    worker.stdin = new EventEmitter();
    worker.stdin.write = (line) => {
      const message = JSON.parse(line);
      messages.push(message);
      if (!worker.respond) return true;
      const result = message.operation === "configure"
        ? { profile: "chrome146", identityProfile: "chrome146" }
        : { status: 200, headers: [], body: "" };
      const reply = worker.responseError
        ? { id: message.id, ok: false, error: worker.responseError }
        : { id: message.id, ok: true, result };
      queueMicrotask(() => worker.stdout.write(JSON.stringify(reply) + "\n"));
      return true;
    };
    workers.push(worker);
    return worker;
  });
  syncBuiltinESMExports();
  context.after(() => {
    context.mock.restoreAll();
    syncBuiltinESMExports();
  });
  return { workers, messages };
}

test("unexpected TLS worker close rejects later operations without creating a direct worker", async (context) => {
  const { workers, messages } = mockTlsWorkers(context);
  const transport = new TlsFingerprintTransport({ isolateProxyEnvironment: true });
  context.after(() => transport.close());
  await transport.configure("http://proxy.invalid:3128");
  await transport.fetch("https://auth.openai.com/");
  workers[0].emit("close", 9, null);
  const messageCount = messages.length;

  await assert.rejects(transport.fetch("https://auth.openai.com/"));
  await assert.rejects(transport.configure("http://proxy.invalid:3128"));
  await assert.rejects(transport.resetSession());
  await assert.rejects(transport.generateSentinelTokens({ flow: "password_verify" }));
  assert.equal(workers.length, 1);
  assert.equal(messages.length, messageCount);
});

test("unexpected TLS worker error rejects an active request and all later requests", async (context) => {
  const { workers, messages } = mockTlsWorkers(context);
  const transport = new TlsFingerprintTransport({ isolateProxyEnvironment: true });
  context.after(() => transport.close());
  await transport.configure("http://proxy.invalid:3128");
  workers[0].respond = false;
  const request = transport.fetch("https://auth.openai.com/", { timeoutMs: 100 });
  const rejected = assert.rejects(request, /worker failure/);
  await new Promise(resolve => setImmediate(resolve));
  workers[0].emit("error", new Error("worker failure"));
  await rejected;
  workers[0].respond = true;
  const messageCount = messages.length;

  await assert.rejects(transport.fetch("https://auth.openai.com/", { timeoutMs: 100 }));
  assert.equal(workers.length, 1);
  assert.equal(messages.length, messageCount);
});

test("explicit TLS transport close rejects active requests and cleans up idempotently", async (context) => {
  const { workers, messages } = mockTlsWorkers(context);
  const transport = new TlsFingerprintTransport({ isolateProxyEnvironment: true });
  context.after(() => transport.close());
  await transport.configure("http://proxy.invalid:3128");
  workers[0].respond = false;
  const request = transport.fetch("https://auth.openai.com/", { timeoutMs: 100 });
  const rejected = assert.rejects(request, /closed/);
  await new Promise(resolve => setImmediate(resolve));
  await transport.close();
  await rejected;
  const messageCount = messages.length;

  await transport.close();
  await assert.rejects(transport.fetch("https://auth.openai.com/"));
  assert.equal(transport.child, null);
  assert.equal(workers.length, 1);
  assert.equal(messages.length, messageCount);
});

test("TLS worker stdin errors reject an active request and prevent later requests", async (context) => {
  const { workers, messages } = mockTlsWorkers(context);
  const transport = new TlsFingerprintTransport({ isolateProxyEnvironment: true });
  context.after(() => transport.close());
  await transport.configure("http://proxy.invalid:3128");
  workers[0].respond = false;
  const request = transport.fetch("https://auth.openai.com/", { timeoutMs: 100 });
  const rejected = assert.rejects(request, /stdin failure/);
  await new Promise(resolve => setImmediate(resolve));
  workers[0].stdin.emit("error", new Error("stdin failure"));
  await rejected;
  const messageCount = messages.length;

  await assert.rejects(transport.fetch("https://auth.openai.com/"));
  assert.equal(workers.length, 1);
  assert.equal(messages.length, messageCount);
});

test("closing a TLS transport while a request awaits its worker prevents the write", async (context) => {
  const { workers, messages } = mockTlsWorkers(context);
  const transport = new TlsFingerprintTransport({ isolateProxyEnvironment: true });
  context.after(() => transport.close());
  await transport.configure("http://proxy.invalid:3128");
  const messageCount = messages.length;
  const request = transport.fetch("https://auth.openai.com/");
  const rejected = assert.rejects(request, /closed/);
  await transport.close();
  await rejected;

  assert.equal(workers.length, 1);
  assert.equal(messages.length, messageCount);
});

test("ordinary TLS request failures preserve the selected proxy for the next request", async (context) => {
  const { workers, messages } = mockTlsWorkers(context);
  const transport = new TlsFingerprintTransport({ isolateProxyEnvironment: true });
  context.after(() => transport.close());
  const proxy = "http://proxy.invalid:3128";
  await transport.configure(proxy);
  workers[0].responseError = "ProxyError: controlled proxy unavailable";
  await assert.rejects(transport.fetch("https://auth.openai.com/"), /controlled proxy unavailable/);
  workers[0].responseError = null;
  const response = await transport.fetch("https://auth.openai.com/");

  assert.equal(response.status, 200);
  assert.equal(workers.length, 1);
  assert.equal(transport.hasConfiguredProxy(), true);
  assert.equal(transport.configuredProxy, proxy);
  assert.deepEqual(messages.filter(message => message.operation === "configure").map(message => message.proxy), [proxy]);
  assert.equal(messages.filter(message => message.operation === "request").length, 2);
});

for (const failure of [
  { name: "stdout end", trigger: worker => worker.stdout.end() },
  { name: "stdout error", trigger: worker => worker.stdout.emit("error", new Error("stdout failure")) },
  { name: "stderr error", trigger: worker => worker.stderr.emit("error", new Error("stderr failure")) },
  { name: "invalid stdout JSON", trigger: worker => worker.stdout.write("invalid-json\n") },
]) {
  test(`TLS worker ${failure.name} rejects every active request and prevents later writes`, async (context) => {
    const { workers, messages } = mockTlsWorkers(context);
    const transport = new TlsFingerprintTransport({ isolateProxyEnvironment: true });
    context.after(() => transport.close());
    await transport.configure("http://proxy.invalid:3128");
    workers[0].respond = false;
    const rejected = Promise.all([
      assert.rejects(transport.fetch("https://auth.openai.com/first", { timeoutMs: 100 })),
      assert.rejects(transport.fetch("https://auth.openai.com/second", { timeoutMs: 100 })),
    ]);
    await new Promise(resolve => setImmediate(resolve));
    const messageCount = messages.length;
    failure.trigger(workers[0]);
    await new Promise(resolve => setImmediate(resolve));

    assert.equal(transport.closed, true);
    await rejected;
    await assert.rejects(transport.fetch("https://auth.openai.com/after-failure"));
    assert.equal(workers.length, 1);
    assert.equal(messages.length, messageCount);
  });
}

test("TLS worker write callback errors reject every active request and prevent later writes", async (context) => {
  const { workers, messages } = mockTlsWorkers(context);
  const transport = new TlsFingerprintTransport({ isolateProxyEnvironment: true });
  context.after(() => transport.close());
  await transport.configure("http://proxy.invalid:3128");
  workers[0].respond = false;
  const firstRejected = assert.rejects(transport.fetch("https://auth.openai.com/first", { timeoutMs: 100 }), /write failure/);
  await new Promise(resolve => setImmediate(resolve));
  workers[0].stdin.write = (_line, callback) => {
    queueMicrotask(() => callback(new Error("write failure")));
    return true;
  };
  const secondRejected = assert.rejects(transport.fetch("https://auth.openai.com/second", { timeoutMs: 100 }), /write failure/);
  await Promise.all([firstRejected, secondRejected]);
  const messageCount = messages.length;

  await assert.rejects(transport.fetch("https://auth.openai.com/after-failure"));
  assert.equal(workers.length, 1);
  assert.equal(messages.length, messageCount);
});
