// The browser owns cookies. This client never accepts OAuth credentials.
const own = (value, key) => Object.prototype.hasOwnProperty.call(value, key);
const encoder = new TextEncoder();
const maxRequest = 1 << 20;
const maxResponse = 4 << 20;
let activeRequests = 0;
export class SDKError extends Error {
  constructor(code, status = 0, apiCode) {
    super("SDK request failed: " + code);
    this.name = "SDKError";
    this.code = code;
    this.status = status;
    this.apiCode = contract.errorCodes.includes(apiCode) ? apiCode : undefined;
  }
}
function fail(code, status = 0) { throw new SDKError(code, status); }
function unicode(value) {
  for (let i = 0; i < value.length; i++) {
    const c = value.charCodeAt(i);
    if (c >= 0xd800 && c <= 0xdbff) {
      const next = value.charCodeAt(++i);
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false;
    } else if (c >= 0xdc00 && c <= 0xdfff) return false;
  }
  return true;
}
// Bound parsing and reject duplicate keys, malformed Unicode, and unsafe integers.
function parseJSON(text) {
  let at = 0, nodes = 0;
  const space = () => { while (at < text.length && /[\x20\t\r\n]/.test(text[at])) at++; };
  const string = () => {
    const start = at++;
    while (at < text.length) {
      const c = text[at++];
      if (c === "\\") { at++; continue; }
      if (c === '"') { const value = JSON.parse(text.slice(start, at)); if (!unicode(value)) fail("invalid_response"); return value; }
    }
    fail("invalid_response");
  };
  const value = (depth) => {
    space(); if (++nodes > 10000 || depth > 32) fail("invalid_response");
    const c = text[at];
    if (c === '"') return string();
    if (c === "{" || c === "[") {
      const object = c === "{", out = object ? Object.create(null) : [];
      const close = object ? "}" : "]";
      at++; space(); if (text[at] === close) { at++; return out; }
      while (at < text.length) {
        if (object) {
          if (text[at] !== '"') fail("invalid_response");
          const key = string(); space();
          if (own(out, key) || text[at++] !== ":") fail("invalid_response");
          out[key] = value(depth + 1);
        } else out.push(value(depth + 1));
        space(); const next = text[at++];
        if (next === close) return out;
        if (next !== ",") fail("invalid_response");
        space();
      }
      fail("invalid_response");
    }
    for (const [word, result] of [["true", true], ["false", false], ["null", null]]) {
      if (text.startsWith(word, at)) { at += word.length; return result; }
    }
    const token = /^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/.exec(text.slice(at));
    if (!token || token[0].length > 128) fail("invalid_response");
    at += token[0].length;
    const number = Number(token[0]);
    if (!Number.isFinite(number) || (Number.isInteger(number) && !Number.isSafeInteger(number))) fail("invalid_response");
    // A non-integer decimal must not become an integer through rounding.
    if (Number.isInteger(number)) {
      const [mantissa, exponent = "0"] = token[0].toLowerCase().split("e");
      const [whole, fraction = ""] = mantissa.split(".");
      const scale = fraction.length - Number(exponent);
      if (scale > 0 && (scale > 400 || BigInt(whole + fraction) % (10n ** BigInt(scale)) !== 0n)) fail("invalid_response");
    }
    return number;
  };
  const result = value(0); space(); if (at !== text.length) fail("invalid_response"); return result;
}
function dateTime(value) {
  const m = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:[Zz]|[+-](\d{2}):(\d{2}))$/.exec(value);
  if (!m) return false;
  const [year, month, day, hour, minute, second] = m.slice(1, 7).map(Number);
  const days = [31, year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0) ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return month >= 1 && month <= 12 && day >= 1 && day <= days[month - 1] && hour < 24 && minute < 60 && second <= 60 && (m[7] === undefined || (Number(m[7]) < 24 && Number(m[8]) < 60));
}
function annotation(schema) {
  while (schema?.$ref) schema = contract.models[schema.$ref.slice("#/components/schemas/".length)];
  return schema;
}
function validate(schema, data, direction, depth = 0, budget = { nodes: 100000, chars: 8 * maxResponse }) {
  if (!schema || data === undefined || depth > 48 || --budget.nodes < 0) return false;
  if (schema.$ref) return validate(contract.models[schema.$ref.slice("#/components/schemas/".length)], data, direction, depth + 1, budget);
  if (schema.allOf && !schema.allOf.every(s => validate(s, data, direction, depth + 1, budget))) return false;
  if (schema.anyOf && !schema.anyOf.some(s => validate(s, data, direction, depth + 1, budget))) return false;
  if (schema.oneOf && schema.oneOf.filter(s => validate(s, data, direction, depth + 1, budget)).length !== 1) return false;
  if (schema.enum && !schema.enum.some(v => JSON.stringify(v) === JSON.stringify(data))) return false;
  const type = schema.type;
  if (data === null) return type === undefined || schema.nullable === true;
  if (type === "string") {
    if (typeof data !== "string" || !unicode(data)) return false;
    let length = 0; for (const _ of data) { length++; if (--budget.chars < 0) return false; }
    if ((schema.minLength !== undefined && length < schema.minLength) || (schema.maxLength !== undefined && length > schema.maxLength)) return false;
    if (schema.format === "date-time" && !dateTime(data)) return false;
    if (schema.format === "uri") { try { if (/[^A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=%]/.test(data) || /%(?![0-9A-Fa-f]{2})/.test(data)) return false; new URL(data); } catch { return false; } }
  } else if (type === "number" || type === "integer") {
    if (typeof data !== "number" || !Number.isFinite(data) || (type === "integer" && !Number.isSafeInteger(data))) return false;
    if (schema.format === "int32" && (!Number.isInteger(data) || data < -2147483648 || data > 2147483647)) return false;
    if (schema.minimum !== undefined && (data < schema.minimum || (schema.exclusiveMinimum && data === schema.minimum))) return false;
    if (schema.maximum !== undefined && (data > schema.maximum || (schema.exclusiveMaximum && data === schema.maximum))) return false;
  } else if (type === "boolean") { if (typeof data !== "boolean") return false; }
  else if (type === "array") {
    if (!Array.isArray(data) || (schema.minItems !== undefined && data.length < schema.minItems) || (schema.maxItems !== undefined && data.length > schema.maxItems)) return false;
    if (!data.every(v => validate(schema.items, v, direction, depth + 1, budget))) return false;
  } else if (type === "object") {
    if (typeof data !== "object" || Array.isArray(data)) return false;
    const keys = Object.keys(data), props = schema.properties ?? {};
    if ((schema.minProperties !== undefined && keys.length < schema.minProperties) || (schema.maxProperties !== undefined && keys.length > schema.maxProperties)) return false;
    for (const name of schema.required ?? []) {
      const field = annotation(props[name]);
      if (field && ((direction === "request" && field.readOnly) || (direction === "response" && field.writeOnly))) continue;
      if (!own(data, name)) return false;
    }
    for (const name of keys) {
      if (own(props, name)) {
        const field = annotation(props[name]);
        if ((direction === "request" && field.readOnly) || (direction === "response" && field.writeOnly) || !validate(field, data[name], direction, depth + 1, budget)) return false;
      } else if (schema.additionalProperties === false || (typeof schema.additionalProperties === "object" && !validate(schema.additionalProperties, data[name], direction, depth + 1, budget))) return false;
    }
  }
  return true;
}
function boundedInput(value, depth = 0, count = { nodes: 0, text: 0 }) {
  if (++count.nodes > 10000 || depth > 32) fail("invalid_input");
  if (value === null || typeof value === "boolean") return;
  if (typeof value === "string") { count.text += value.length; if (count.text > maxRequest || !unicode(value)) fail("invalid_input"); return; }
  if (typeof value === "number") { if (!Number.isFinite(value) || (Number.isInteger(value) && !Number.isSafeInteger(value))) fail("invalid_input"); return; }
  if (typeof value !== "object") fail("invalid_input");
  if (!Array.isArray(value) && Object.getPrototypeOf(value) !== Object.prototype && Object.getPrototypeOf(value) !== null) fail("invalid_input");
  if (Array.isArray(value)) { if (value.length > 10000 || Object.keys(value).length !== value.length) fail("invalid_input"); for (let i = 0; i < value.length; i++) if (!own(value, i)) fail("invalid_input"); }
  for (const key of Object.keys(value)) { count.text += key.length; if (count.text > maxRequest || !unicode(key)) fail("invalid_input"); boundedInput(value[key], depth + 1, count); }
}
export function createBrowserClient() {
  let origin, fetcher;
  try { const url = new URL(globalThis.location.origin); if (url.protocol !== "https:" || url.username || url.password || url.pathname !== "/" || url.search || url.hash) fail("invalid_origin"); origin = url.origin; fetcher = globalThis.fetch.bind(globalThis); } catch { fail("invalid_origin"); }
  const run = async (options, action) => {
    if (activeRequests >= 16) fail("busy");
    activeRequests++;
    const controller = new AbortController(); let timedOut = false, supplied;
    const timer = setTimeout(() => { timedOut = true; controller.abort(); }, 20000);
    const abort = () => controller.abort();
    try {
      if (options !== undefined && (options === null || typeof options !== "object" || Object.keys(options).some(k => k !== "signal"))) fail("invalid_input");
      supplied = options?.signal;
      if (supplied !== undefined) { if (!(supplied instanceof AbortSignal)) fail("invalid_input"); supplied.addEventListener("abort", abort, { once: true }); if (supplied.aborted) controller.abort(); }
      if (controller.signal.aborted) fail("cancelled");
      return await action(controller.signal);
    } catch (error) { if (error instanceof SDKError) throw error; fail(timedOut ? "timeout" : controller.signal.aborted ? "cancelled" : "request_failed"); }
    finally { clearTimeout(timer); activeRequests--; try { supplied?.removeEventListener("abort", abort); } catch {} }
  };
  const request = async (method, path, body, csrf, signal) => {
    const target = new URL(path, origin);
    if (target.origin !== origin || target.username || target.password || target.hash || target.href.length > 8192) fail("invalid_input");
    const headers = { Accept: "application/json" };
    if (body !== undefined) { headers["Content-Type"] = "application/json"; if (encoder.encode(body).length > maxRequest) fail("invalid_input"); }
    if (csrf !== undefined) headers["X-CSRF-Token"] = csrf;
    const response = await fetcher(target.href, { method, headers, body, signal, credentials: "same-origin", mode: "same-origin", cache: "no-store", redirect: "error", referrerPolicy: "no-referrer" });
    const reader = response.body?.getReader(); let data = "", length = 0;
    const decoder = new TextDecoder("utf-8", { fatal: true });
    if (reader) { try { while (true) { const chunk = await reader.read(); if (chunk.done) break; length += chunk.value.byteLength; if (length > maxResponse) fail("response_limit"); data += decoder.decode(chunk.value, { stream: true }); } data += decoder.decode(); } finally { try { await reader.cancel(); } finally { reader.releaseLock(); } } }
    if (!response.ok) {
      let apiCode;
      if (response.status !== 401 && (response.headers.get("Content-Type") ?? "").split(";")[0].trim().toLowerCase() === "application/json") {
        try { const value = parseJSON(data); if (value && typeof value === "object" && own(value, "code") && contract.errorCodes.includes(value.code)) apiCode = value.code; } catch {}
      }
      throw new SDKError(response.status === 401 ? "reauth_required" : "http_error", response.status, apiCode);
    }
    let result;
    if (data !== "") { if ((response.headers.get("Content-Type") ?? "").split(";")[0].trim().toLowerCase() !== "application/json") fail("invalid_response"); try { result = parseJSON(data); } catch { fail("invalid_response"); } }
    return { status: response.status, body: result, etag: response.headers.get("ETag") };
  };
  const session = async signal => {
    const result = await request("GET", "/auth/session", undefined, undefined, signal);
    const value = result.body;
    if (result.status !== 200 || !value || typeof value.authenticated !== "boolean" || !Array.isArray(value.roles) || value.roles.length > 64 || value.roles.some(r => typeof r !== "string" || r.length > 128)) fail("invalid_response");
    if (value.authenticated && (typeof value.csrf_token !== "string" || !/^[A-Za-z0-9_-]{43}$/.test(value.csrf_token))) fail("invalid_response");
    const allowed = new Set(["authenticated", "roles", "user", "expires_at", "csrf_token"]);
    if (Object.keys(value).some(k => !allowed.has(k))) fail("invalid_response");
    if (own(value, "expires_at") && (!Number.isSafeInteger(value.expires_at) || value.expires_at < 0)) fail("invalid_response");
    if (own(value, "user") && (!value.user || typeof value.user !== "object" || Array.isArray(value.user) || Object.keys(value.user).some(k => !["sub", "email", "name", "preferred_username"].includes(k) || typeof value.user[k] !== "string" || value.user[k].length > 1024))) fail("invalid_response");
    if (value.authenticated && (!own(value, "expires_at") || !value.user || typeof value.user.sub !== "string" || !value.user.sub)) fail("invalid_response");
    return value;
  };
  const client = Object.create(null);
  client.login = () => {
    let returnTo = "/";
    try {
      const location = globalThis.location;
      const candidate = String(location.pathname ?? "/") + String(location.search ?? "") + String(location.hash ?? "");
      const target = new URL(candidate, origin);
      if (candidate.startsWith("/") && !candidate.startsWith("//") && !/[\\\r\n\x00]/.test(candidate) && candidate.length <= 2048 && target.origin === origin) returnTo = candidate;
      const login = new URL("/auth/login", origin);
      login.searchParams.set("return_to", returnTo);
      location.assign(login.href);
    } catch { fail("navigation_failed"); }
  };
  client.session = options => run(options, async signal => { const value = await session(signal); const { csrf_token, ...publicValue } = value; return publicValue; });
  client.logout = options => run(options, async signal => { const value = await session(signal); if (!value.authenticated) return; const result = await request("POST", "/auth/logout", undefined, value.csrf_token, signal); if (result.status !== 204) fail("invalid_response"); });
  for (const [name, op] of Object.entries(contract.operations)) {
    client[name] = (input = {}, options) => run(options, async signal => {
      boundedInput(input);
      if (input === null || typeof input !== "object" || Array.isArray(input)) fail("invalid_input");
      const allowed = new Set(op.parameters.map(p => p.name)); if (op.body) allowed.add("body");
      if (Object.keys(input).some(key => !allowed.has(key))) fail("invalid_input");
      let path = op.path; const query = new URLSearchParams();
      for (const param of op.parameters) {
        if (!own(input, param.name)) { if (param.required) fail("invalid_input"); continue; }
        const value = input[param.name]; if (!validate(param.schema, value, "request")) fail("invalid_input");
        if (param.in === "path") { const text = String(value); if (!text || text === "." || text === "..") fail("invalid_input"); path = path.split("{" + param.name + "}").join(encodeURIComponent(text)); }
        else query.set(param.name, String(value));
      }
      if (op.required && !own(input, "body")) fail("invalid_input");
      if (own(input, "body") && (!op.body || !validate(op.body, input.body, "request"))) fail("invalid_input");
      const suffix = query.toString(); if (suffix) path += "?" + suffix;
      const target = new URL(path, origin); if (target.origin !== origin || target.href.length > 8192) fail("invalid_input");
      const body = own(input, "body") ? JSON.stringify(input.body) : undefined; if (body !== undefined && encoder.encode(body).length > maxRequest) fail("invalid_input");
      let csrf;
      if (op.method !== "GET" && op.method !== "HEAD") { const value = await session(signal); if (!value.authenticated) fail("reauth_required", 401); csrf = value.csrf_token; }
      const result = await request(op.method, path, body, csrf, signal);
      if (!own(op.responses, String(result.status))) fail("invalid_response");
      const schema = op.responses[String(result.status)];
      if (schema === null ? result.body !== undefined : !validate(schema, result.body, "response")) fail("invalid_response");
      return Object.freeze(result);
    });
  }
  return Object.freeze(client);
}
