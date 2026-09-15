import assert from 'node:assert/strict';
import {test} from 'node:test';
import {createBrowserTelemetry,RootTraceIdGenerator} from './telemetry/index.js';
const csrf='c'.repeat(43);
const parent='0af7651916cd43dd8448eb211c80319c';
function setup(handler) {
 globalThis.location={href:'https://console.example/gateways'};
 globalThis.document=new EventTarget();
 globalThis.window=new EventTarget();
 const calls=[];
 globalThis.fetch=async (url,options)=>{
  calls.push({url,options});
  assert.equal(options.redirect,'error');assert.equal(options.credentials,'same-origin');assert.equal(options.mode,'same-origin');
  if (url.endsWith('/auth/session')) return Response.json({authenticated:true,csrf_token:csrf});
  assert.equal(options.headers['X-CSRF-Token'],csrf);
  return handler ? handler(url,options) : new Response(null);
 };
 return calls;
}
test('all signals use the fixed authenticated transport and generated identity', async()=>{
 const calls=setup();const failures=[];const t=createBrowserTelemetry({sampleRatio:1,reportDeliveryFailure:f=>failures.push(f)});
 try {
  t.beginTrace(parent);const span=t.tracer.startSpan('workflow.create');span.end();
  t.logger.emit({body:'workflow.created',severityNumber:9});
  t.meter.createCounter('workflow.created').add(1);
  await t.forceFlush();
  const posts=calls.filter(c=>c.options.method==='POST');
  assert.deepEqual(posts.map(c=>c.url).sort(),['logs','metrics','traces'].map(s=>'https://console.example/telemetry/v1/'+s).sort());
  for (const post of posts){const body=Buffer.from(post.options.body);assert.ok(body.includes(Buffer.from('example-console')));assert.equal(body.includes(Buffer.from('csrf_token')),false);assert.equal(post.options.headers['Content-Type'],'application/x-protobuf');}
  const trace=Buffer.from(posts.find(c=>c.url.endsWith('/traces')).options.body);
  assert.ok(trace.includes(Buffer.from(parent,'hex')));
  assert.deepEqual(failures,[]);
 }finally{await t.shutdown();}
});
test('invalid configuration and trace IDs fail before export',async()=>{
 const calls=setup();
 for(const options of [null,[],{url:'https://elsewhere.example'}, {sampleRatio:-1},{sampleRatio:2},{sampleRatio:NaN},{reportDeliveryFailure:4}])assert.throws(()=>createBrowserTelemetry(options));
 const ids=new RootTraceIdGenerator();
 for(const id of ['',parent+'\n',parent.toUpperCase(),'0'.repeat(32)])assert.throws(()=>ids.primeTraceId(id));
 assert.match(ids.generateSpanId(),/^[0-9a-f]{16}$/);assert.equal(calls.length,0);
 globalThis.location.href='http://console.example';assert.throws(()=>createBrowserTelemetry());
});
test('export failures are bounded public categories, including partial success',async()=>{
 for(const reply of [()=>new Response('private secret',{status:500}),()=>new Response(new Uint8Array([10,2,8,1]),{headers:{'Content-Type':'application/x-protobuf'}}),()=>new Response('x'.repeat(16385),{headers:{'Content-Type':'application/json'}})]){
  setup(reply);const failures=[];const t=createBrowserTelemetry({sampleRatio:1,reportDeliveryFailure:f=>failures.push(f)});
  t.tracer.startSpan('workflow').end();await assert.rejects(t.forceFlush(),/telemetry flush failed/);await t.shutdown().catch(()=>{});
  assert.ok(failures.some(f=>f.signal==='traces'&&f.reason==='export_failed'));
  assert.doesNotMatch(JSON.stringify(failures),/private|secret/);
 }
});
test('authentication failure makes no export request and reporter throws do not escape',async()=>{
 setup();let posts=0;globalThis.fetch=async(url,options)=>{if(options.method==='POST')posts++;return Response.json({authenticated:false});};
 const t=createBrowserTelemetry({sampleRatio:1,reportDeliveryFailure:()=>{throw new Error('private');}});
 t.tracer.startSpan('workflow').end();await assert.rejects(t.forceFlush(),/telemetry flush failed/);await t.shutdown().catch(()=>{});assert.equal(posts,0);
});
test('page hide flushes and shutdown is idempotent and removes listeners',async()=>{
 const calls=setup();const t=createBrowserTelemetry({sampleRatio:1});
 t.tracer.startSpan('workflow').end();globalThis.window.dispatchEvent(new Event('pagehide'));
 await t.forceFlush();assert.ok(calls.some(c=>c.url.endsWith('/traces')));
 const done=t.shutdown();assert.equal(t.shutdown(),done);await done;
 const count=calls.length;globalThis.window.dispatchEvent(new Event('pagehide'));await new Promise(resolve=>setTimeout(resolve,10));assert.equal(calls.length,count);
});
test('a stalled fetch has a terminal result',async()=>{
 setup();globalThis.fetch=()=>new Promise(()=>{});const failures=[];const t=createBrowserTelemetry({sampleRatio:1,reportDeliveryFailure:f=>failures.push(f)});
 t.tracer.startSpan('workflow').end();const started=Date.now();await assert.rejects(t.forceFlush(),/telemetry flush failed/);await t.shutdown().catch(()=>{});
 assert.ok(Date.now()-started<6500);assert.ok(failures.some(f=>f.signal==='traces'));
});

test('log bodies, metric instruments, and attributes have finite limits',async()=>{
 const calls=setup();const failures=[];const t=createBrowserTelemetry({sampleRatio:0,reportDeliveryFailure:f=>failures.push(f)});
 t.logger.emit({body:'x'.repeat(513)});t.logger.emit({body:{secret:'not exported'}});
 for(let i=0;i<33;i++)t.meter.createCounter('counter.'+i).add(1);
 t.meter.createCounter('counter.0').add(1,{large:'x'.repeat(257)});
 await t.forceFlush();await t.shutdown();
 assert.equal(calls.filter(c=>c.url.endsWith('/logs')).length,0);
 assert.equal(failures.filter(f=>f.signal==='logs'&&f.reason==='invalid_record').length,2);
 assert.equal(failures.filter(f=>f.signal==='metrics'&&f.reason==='invalid_record').length,2);
});

test('observable metric callbacks enforce record limits and can be removed',async()=>{
 const calls=setup();const failures=[];const t=createBrowserTelemetry({sampleRatio:0,reportDeliveryFailure:f=>failures.push(f)});
 const registrations=[];let observations=0;
 try {
  for(const kind of ['Gauge','Counter','UpDownCounter']){
   const instrument=t.meter['createObservable'+kind]('observable.'+kind);
   const callback=result=>{
    observations++;
    result.observe(1,{state:'valid'});
    result.observe(2,{state:'oversized-'+ 'x'.repeat(257)});
    result.observe(3,{state:['invalid-array']});
    result.observe(Infinity);
   };
   instrument.addCallback(callback);instrument.addCallback(callback);
   registrations.push([instrument,callback]);
  }
  await t.forceFlush();
  assert.equal(observations,3);
  assert.equal(failures.filter(f=>f.signal==='metrics'&&f.reason==='invalid_record').length,9);
  const payload=Buffer.concat(calls.filter(c=>c.url.endsWith('/metrics')).map(c=>Buffer.from(c.options.body)));
  assert.ok(payload.includes(Buffer.from('valid')));
  assert.equal(payload.includes(Buffer.from('oversized-')),false);
  assert.equal(payload.includes(Buffer.from('invalid-array')),false);
  for(const [instrument,callback] of registrations)instrument.removeCallback(callback);
  await t.forceFlush();assert.equal(observations,3);
 }finally{for(const [instrument,callback] of registrations)instrument.removeCallback(callback);await t.shutdown();}
});

test('batch observable metric callbacks enforce record limits and can be removed',async()=>{
 const calls=setup();const failures=[];const t=createBrowserTelemetry({sampleRatio:0,reportDeliveryFailure:f=>failures.push(f)});
 const gauge=t.meter.createObservableGauge('batch.gauge');const counter=t.meter.createObservableCounter('batch.counter');let observations=0;
 const callback=result=>{
  observations++;
  result.observe(gauge,1,{state:'valid-gauge'});result.observe(counter,2,{state:'valid-counter'});
  result.observe(gauge,3,{state:'oversized-'+ 'x'.repeat(257)});result.observe(counter,NaN);
 };
 try {
  t.meter.addBatchObservableCallback(callback,[gauge,counter]);
  t.meter.addBatchObservableCallback(callback,[counter,gauge,gauge]);
  await t.forceFlush();assert.equal(observations,1);
  assert.equal(failures.filter(f=>f.signal==='metrics'&&f.reason==='invalid_record').length,2);
  const payload=Buffer.concat(calls.filter(c=>c.url.endsWith('/metrics')).map(c=>Buffer.from(c.options.body)));
  assert.ok(payload.includes(Buffer.from('valid-gauge')));assert.ok(payload.includes(Buffer.from('valid-counter')));
  assert.equal(payload.includes(Buffer.from('oversized-')),false);
  t.meter.removeBatchObservableCallback(callback,[counter,gauge]);
  await t.forceFlush();assert.equal(observations,1);
 }finally{t.meter.removeBatchObservableCallback(callback,[gauge,counter]);await t.shutdown();}
});

test('observable metric callbacks have a shared registration limit and bounded collection',async()=>{
 const calls=setup();const failures=[];const t=createBrowserTelemetry({sampleRatio:0,reportDeliveryFailure:f=>failures.push(f)});
 const gauge=t.meter.createObservableGauge('bounded.gauge');const counter=t.meter.createObservableCounter('bounded.counter');
 const callbacks=Array.from({length:31},()=>()=>{});let batchCalls=0;let extraCalls=0;
 const batch=result=>{batchCalls++;for(let i=0;i<33;i++)result.observe(gauge,i,{series:i===32?'excluded-series':String(i)});};
 const extra=result=>{extraCalls++;result.observe(1);};
 try {
  for(const callback of callbacks)gauge.addCallback(callback);
  t.meter.addBatchObservableCallback(batch,[gauge]);counter.addCallback(extra);
  await t.forceFlush();assert.equal(extraCalls,0);assert.equal(batchCalls,1);
  assert.equal(failures.filter(f=>f.reason==='invalid_record').length,2);
  const payload=Buffer.concat(calls.filter(c=>c.url.endsWith('/metrics')).map(c=>Buffer.from(c.options.body)));
  assert.equal(payload.includes(Buffer.from('excluded-series')),false);
  gauge.removeCallback(callbacks.pop());counter.addCallback(extra);
  t.meter.removeBatchObservableCallback(batch,[gauge]);
  await t.forceFlush();assert.equal(extraCalls,1);assert.equal(batchCalls,1);
 }finally{for(const callback of callbacks)gauge.removeCallback(callback);counter.removeCallback(extra);t.meter.removeBatchObservableCallback(batch,[gauge]);await t.shutdown();}
});

test('observable metric callbacks reject foreign and unselected instruments',async()=>{
 setup();const failures=[];const t=createBrowserTelemetry({sampleRatio:0,reportDeliveryFailure:f=>failures.push(f)});
 const other=createBrowserTelemetry({sampleRatio:0});const local=t.meter.createObservableGauge('local');const unselected=t.meter.createObservableGauge('unselected');const foreign=other.meter.createObservableGauge('foreign');let invalidCalls=0;
 const invalid=()=>{invalidCalls++;};
 const callback=async result=>{await Promise.resolve();result.observe(local,1);result.observe(unselected,1);result.observe(foreign,1);};
 try {
  t.meter.addBatchObservableCallback(invalid,[foreign]);
  t.meter.addBatchObservableCallback(invalid,[t.meter.createCounter('synchronous')]);
  t.meter.addBatchObservableCallback(callback,[local]);
  await t.forceFlush();assert.equal(invalidCalls,0);assert.equal(failures.filter(f=>f.reason==='invalid_record').length,4);
 }finally{t.meter.removeBatchObservableCallback(callback,[local]);await t.shutdown();await other.shutdown();}
});

test('processor queue loss is reported for logs and traces',async()=>{
 setup();const failures=[];const t=createBrowserTelemetry({sampleRatio:1,reportDeliveryFailure:f=>failures.push(f)});
 try {
  for(let i=0;i<300;i++){t.tracer.startSpan('workflow').end();t.logger.emit({body:'workflow.completed'});}
  await t.forceFlush().catch(()=>{});
  assert.ok(failures.some(f=>f.signal==='traces'&&f.reason==='queue_full'));
  assert.ok(failures.some(f=>f.signal==='logs'&&f.reason==='queue_full'));
 }finally{await t.shutdown();}
});

test('an invalid entropy source cannot return zero IDs or loop without a bound',()=>{
 const crypto = Object.getOwnPropertyDescriptor(globalThis,'crypto');
 let calls=0;
 Object.defineProperty(globalThis,'crypto',{configurable:true,value:{getRandomValues: value=>{calls++;value.fill(0);return value;}}});
 try {assert.throws(()=>new RootTraceIdGenerator().generateTraceId(),/entropy/);assert.equal(calls,3);}
 finally {Object.defineProperty(globalThis,'crypto',crypto);}
});

test('server rendering creates no browser transport or timers',async()=>{
 const location=globalThis.location;delete globalThis.location;
 let calls=0;globalThis.fetch=()=>{calls++;throw new Error('unexpected SSR export');};
 try {const t=createBrowserTelemetry({sampleRatio:1});t.tracer.startSpan('render').end();t.logger.emit({body:'render'});t.meter.createCounter('render').add(1);await t.forceFlush();await t.shutdown();assert.equal(calls,0);}
 finally {globalThis.location=location;}
});

test('public deployment settings control each exporter and reject invalid metadata',async()=>{
 for(const settings of [undefined, {version:1,traces:false,logs:false,metrics:false,sampleRatio:0}, {version:1,traces:true,logs:false,metrics:true,sampleRatio:1}, {version:1,traces:true,logs:true,metrics:true,sampleRatio:2}, {version:1,traces:true,logs:true,metrics:true,sampleRatio:1,secret:'invalid'}]){
  const calls=setup();document.querySelectorAll=()=>settings===undefined?[]:[{getAttribute:()=>JSON.stringify(settings)}];
  const t=createBrowserTelemetry();t.tracer.startSpan('deployment').end();t.logger.emit({body:'deployment'});t.meter.createCounter('deployment').add(1);await t.forceFlush();await t.shutdown();
  const actual=[...new Set(calls.filter(c=>c.options.method==='POST').map(c=>new URL(c.url).pathname))].sort();
  const expected=settings?.sampleRatio===1&&settings.logs===false?['/telemetry/v1/metrics','/telemetry/v1/traces']:[];
  assert.deepEqual(actual,expected);
 }
});
