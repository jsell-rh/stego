import { createNoopMeter, ProxyTracerProvider } from '@opentelemetry/api';
import { createNoopLogger } from '@opentelemetry/api-logs';
import { ExportResultCode } from '@opentelemetry/core';
import { resourceFromAttributes } from '@opentelemetry/resources';
import { BasicTracerProvider, BatchSpanProcessor, ParentBasedSampler, TraceIdRatioBasedSampler } from '@opentelemetry/sdk-trace-base';
import { LoggerProvider, BatchLogRecordProcessor } from '@opentelemetry/sdk-logs';
import { MeterProvider, PeriodicExportingMetricReader } from '@opentelemetry/sdk-metrics';
import { ProtobufTraceSerializer, ProtobufLogsSerializer, ProtobufMetricsSerializer } from '@opentelemetry/otlp-transformer';

const maxBody = 262144;
const maxResponse = 16384;
let instances = 0;
function randomID(bytes) {
  const data = new Uint8Array(bytes);
  for (let attempt = 0; attempt < 3; attempt++) {
    globalThis.crypto.getRandomValues(data);
    if (data.some(x => x !== 0)) return Array.from(data, x => x.toString(16).padStart(2, '0')).join('');
  }
  throw new Error('browser entropy source failed');
}
export class RootTraceIdGenerator {
  #next;
  primeTraceId(value) {
    if (typeof value !== 'string' || value.length !== 32 || !/^[0-9a-f]{32}$/.test(value) || value === '0'.repeat(32)) throw new TypeError('invalid trace ID');
    this.#next = value;
  }
  generateTraceId() { const value = this.#next; this.#next = undefined; return value ?? randomID(16); }
  generateSpanId() { return randomID(8); }
}

// Only finite loss categories reach application code. Private errors do not.
function healthMeter(signal, report) {
  const noop = createNoopMeter();
  return { getMeter() { return { ...noop,
    createCounter(name, options) {
      const inner = noop.createCounter(name, options);
      return { add(value, attributes) {
        const error = attributes?.['error.type'];
        if (error && Number.isFinite(value) && value > 0) report(signal, error === 'queue_full' ? 'queue_full' : 'export_failed');
        inner.add(value, attributes);
      } };
    },
    createGauge: (...args) => noop.createGauge(...args),
    createHistogram: (...args) => noop.createHistogram(...args),
    createObservableCounter: (...args) => noop.createObservableCounter(...args),
    createObservableGauge: (...args) => noop.createObservableGauge(...args),
    createObservableUpDownCounter: (...args) => noop.createObservableUpDownCounter(...args),
    createUpDownCounter: (...args) => noop.createUpDownCounter(...args),
    addBatchObservableCallback: (...args) => noop.addBatchObservableCallback(...args),
    removeBatchObservableCallback: (...args) => noop.removeBatchObservableCallback(...args),
  }; } };
}
async function boundedBody(response) {
  const declared = response.headers.get('Content-Length');
  if (declared !== null && (!/^\d+$/.test(declared) || Number(declared) > maxResponse)) { await response.body?.cancel(); throw new Error('response limit'); }
  if (!response.body) return new Uint8Array();
  const reader = response.body.getReader();
  const parts = []; let size = 0;
  try {
    for (;;) {
      const {value, done} = await reader.read(); if (done) break;
      size += value.byteLength;
      if (size > maxResponse) throw new Error('response limit');
      parts.push(value);
    }
  } catch (error) { await reader.cancel().catch(() => {}); throw error; }
  finally { reader.releaseLock(); }
  const bytes = new Uint8Array(size); let offset = 0;
  for (const part of parts) { bytes.set(part, offset); offset += part.byteLength; }
  return bytes;
}
function exporter(signal, serializer, origin, fetcher, report) {
  let stopped = false;
  const pending = new Set();
  const controllers = new Set();
  const send = async data => {
    const controller = new AbortController(); controllers.add(controller);
    let timer;
    const deadline = new Promise((_, reject) => { timer = setTimeout(() => { controller.abort(); reject(new Error('export deadline')); }, 5000); });
    try {
      await Promise.race([deadline, (async () => {
        const request = async (path, init = {}) => {
          const response = await fetcher(origin + path, { ...init, signal: controller.signal, credentials: 'same-origin', mode: 'same-origin', cache: 'no-store', redirect: 'error', referrerPolicy: 'no-referrer' });
          if (!response.ok) { await response.body?.cancel(); throw new Error('export response'); }
          return response;
        };
        const sessionResponse = await request('/auth/session', {headers: {Accept: 'application/json'}});
        if ((sessionResponse.headers.get('Content-Type') ?? '').split(';')[0].trim().toLowerCase() !== 'application/json') throw new Error('session type');
        const session = JSON.parse(new TextDecoder('utf-8', {fatal:true}).decode(await boundedBody(sessionResponse)));
        if (session.authenticated !== true || typeof session.csrf_token !== 'string' || session.csrf_token.length !== 43 || !/^[A-Za-z0-9_-]+$/.test(session.csrf_token)) throw new Error('session required');
        const response = await request('/telemetry/v1/' + signal, {method: 'POST', headers: {'Content-Type':'application/x-protobuf', Accept:'application/x-protobuf', 'X-CSRF-Token':session.csrf_token}, body:data});
        const body = await boundedBody(response);
        if (body.length) {
          if ((response.headers.get('Content-Type') ?? '').split(';')[0].trim().toLowerCase() !== 'application/x-protobuf') throw new Error('export type');
          const result = serializer.deserializeResponse(body);
          const rejected = result?.partialSuccess;
          if (rejected && (Number(rejected.rejectedSpans ?? 0) || Number(rejected.rejectedLogRecords ?? 0) || Number(rejected.rejectedDataPoints ?? 0) || rejected.errorMessage)) throw new Error('partial export');
        }
      })()]);
    } finally { clearTimeout(timer); controller.abort(); controllers.delete(controller); }
  };
  return {
    export(records, callback) {
      let done = false;
      const finish = code => { if (done) return; done = true; if (code !== ExportResultCode.SUCCESS) report(signal, 'export_failed'); callback({code}); };
      if (stopped || pending.size >= 1) { finish(ExportResultCode.FAILED); return; }
      let data;
      try { data = serializer.serializeRequest(records); if (!(data instanceof Uint8Array) || data.byteLength > maxBody) throw new Error('export limit'); }
      catch { finish(ExportResultCode.FAILED); return; }
      const task = send(data).then(() => finish(ExportResultCode.SUCCESS), () => finish(ExportResultCode.FAILED));
      pending.add(task); void task.then(() => pending.delete(task), () => pending.delete(task));
    },
    forceFlush: async () => { await Promise.allSettled([...pending]); },
    shutdown: async () => { stopped = true; await Promise.allSettled([...pending]); for (const controller of controllers) controller.abort(); },
  };
}

function validAttributes(attributes) {
  if (attributes === undefined) return true;
  if (!attributes || typeof attributes !== 'object' || Array.isArray(attributes)) return false;
  const entries = Object.entries(attributes);
  return entries.length <= 16 && entries.every(([key,value]) => key.length <= 64 && key.length > 0 &&
    (typeof value === 'string' ? value.length <= 256 : typeof value === 'boolean' || (typeof value === 'number' && Number.isFinite(value))));
}
function boundedMeter(inner, report) {
  const noop = createNoopMeter(); const instruments = new Map();
  return new Proxy(inner, {get(target,key) {
    const method = target[key];
    if (typeof key !== 'string' || !key.startsWith('create') || typeof method !== 'function') return typeof method === 'function' ? method.bind(target) : method;
    return (name, options) => {
      const id = key + ':' + name;
      if (typeof name !== 'string' || !/^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$/.test(name) || (!instruments.has(id) && instruments.size >= 32)) { report('metrics','invalid_record'); return noop[key]('stego.invalid'); }
      if (instruments.has(id)) return instruments.get(id);
      const instrument = method.call(target,name,options);
      const bounded = new Proxy(instrument,{get(value,property) {
        const action = value[property];
        if (property !== 'add' && property !== 'record') return typeof action === 'function' ? action.bind(value) : action;
        return (number,attributes,context) => { if (!Number.isFinite(number) || !validAttributes(attributes)) { report('metrics','invalid_record'); return; } action.call(value,number,attributes,context); };
      }});
      instruments.set(id,bounded);return bounded;
    };
  }});
}
function boundedLogger(inner, report) {
  return {emit(record) {
    if (!record || (record.body !== undefined && (typeof record.body !== 'string' || record.body.length > 512)) || !validAttributes(record.attributes)) { report('logs','invalid_record'); return; }
    inner.emit(record);
  }};
}

export function createBrowserTelemetry(options = {}) {
  if (!options || typeof options !== 'object' || Array.isArray(options) || Object.keys(options).some(k => k !== 'sampleRatio' && k !== 'reportDeliveryFailure')) throw new TypeError('invalid telemetry options');
  const ratio = options.sampleRatio ?? 0;
  const callback = options.reportDeliveryFailure;
  if (!Number.isFinite(ratio) || ratio < 0 || ratio > 1 || (callback !== undefined && typeof callback !== 'function')) throw new TypeError('invalid telemetry options');
  if (typeof globalThis.location === 'undefined') {
    // Server rendering does not create browser exporters or global providers.
    return Object.freeze({tracer:new ProxyTracerProvider().getTracer('stego.browser'),meter:createNoopMeter(),logger:createNoopLogger(),beginTrace() {},forceFlush:async()=>{},shutdown:async()=>{}});
  }
  const url = new URL(globalThis.location?.href);
  if (url.protocol !== 'https:' || url.username || url.password || typeof globalThis.fetch !== 'function' || !globalThis.crypto?.getRandomValues || instances >= 4) throw new TypeError('browser telemetry requires a bounded HTTPS runtime');
  const origin = url.origin;
  const fetcher = globalThis.fetch.bind(globalThis);
  const report = (signal, reason) => { try { callback?.(Object.freeze({signal, reason})); } catch {} };
  const resource = resourceFromAttributes({'service.name':serviceName});
  const idGenerator = new RootTraceIdGenerator();
  const traces = exporter('traces', ProtobufTraceSerializer, origin, fetcher, report);
  const logs = exporter('logs', ProtobufLogsSerializer, origin, fetcher, report);
  const metrics = exporter('metrics', ProtobufMetricsSerializer, origin, fetcher, report);
  // The exporter reports transport loss. The processor meter reports queue loss.
  const queueReport = (signal, reason) => { if (reason === 'queue_full') report(signal, reason); };
  const traceProvider = new BasicTracerProvider({resource, idGenerator,
    sampler:new ParentBasedSampler({root:new TraceIdRatioBasedSampler(ratio)}),
    spanLimits:{attributeCountLimit:16, attributeValueLengthLimit:256, eventCountLimit:2, linkCountLimit:2, attributePerEventCountLimit:8, attributePerLinkCountLimit:8},
    spanProcessors:[new BatchSpanProcessor(traces, {disableAutoFlushOnDocumentHide:true, maxQueueSize:256, maxExportBatchSize:16, scheduledDelayMillis:1000, exportTimeoutMillis:6000, selfObsMeterProvider:healthMeter('traces',queueReport)})],
  });
  const logProvider = new LoggerProvider({resource, logRecordLimits:{attributeCountLimit:16,attributeValueLengthLimit:256},
    processors:[new BatchLogRecordProcessor({exporter:logs,selfObsMeterProvider:healthMeter('logs',queueReport),disableAutoFlushOnDocumentHide:true,maxQueueSize:256,maxExportBatchSize:16,scheduledDelayMillis:1000,exportTimeoutMillis:6000})],
  });
  const meterProvider = new MeterProvider({resource, views:[{instrumentName:'*',aggregationCardinalityLimit:32}], readers:[new PeriodicExportingMetricReader({exporter:metrics,exportIntervalMillis:30000,exportTimeoutMillis:6000})]});
  const providers = [traceProvider, logProvider, meterProvider];
  let shutdownTask; let flushTask;
  const forceFlush = () => {
    if (shutdownTask) return shutdownTask;
    if (!flushTask) flushTask = Promise.allSettled(providers.map(p => p.forceFlush())).then(results => { if (results.some(r => r.status === 'rejected')) throw new Error('telemetry flush failed'); }).finally(() => { flushTask = undefined; });
    return flushTask;
  };
  const flushOnHide = () => { void forceFlush().catch(() => {}); };
  const visibility = () => { if (globalThis.document?.visibilityState === 'hidden') flushOnHide(); };
  globalThis.document?.addEventListener('visibilitychange',visibility);
  globalThis.window?.addEventListener('pagehide',flushOnHide);
  instances++;
  return Object.freeze({
    tracer:traceProvider.getTracer('stego.browser','1.0.0'),
    meter:boundedMeter(meterProvider.getMeter('stego.browser','1.0.0'), report),
    logger:boundedLogger(logProvider.getLogger('stego.browser','1.0.0'), report),
    beginTrace:value => idGenerator.primeTraceId(value),
    forceFlush,
    shutdown() {
      if (!shutdownTask) {
        globalThis.document?.removeEventListener('visibilitychange',visibility);
        globalThis.window?.removeEventListener('pagehide',flushOnHide);
        shutdownTask = (async () => { await flushTask?.catch(() => {}); const results = await Promise.allSettled(providers.map(p => p.shutdown())); if (results.some(r => r.status === 'rejected')) throw new Error('telemetry shutdown failed'); })().finally(() => { instances--; });
      }
      return shutdownTask;
    },
  });
}
