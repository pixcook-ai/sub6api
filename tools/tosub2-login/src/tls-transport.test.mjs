import assert from "node:assert/strict";
import test from "node:test";
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
