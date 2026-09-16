import test from 'node:test';
import assert from 'node:assert/strict';
import {createBrowserClient, SDKError} from './session/index.js';

const csrf='c'.repeat(43);
const json=(body,status=200)=>new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json'}});
const session=()=>json({authenticated:true,roles:['owner'],user:{sub:'fixture'},expires_at:Date.now()+60000,csrf_token:csrf});
const client=fetcher=>{globalThis.location={origin:'https://console.example',pathname:'/workspaces'};globalThis.fetch=fetcher;return createBrowserClient();};
const error=code=>e=>e instanceof SDKError && e.code===code && !e.message.includes('private');

test('upstream JSON writes use the shared session transport',async()=>{
 const calls=[];
 const api=client(async(url,options)=>{
  calls.push({url,options});
  assert.equal(options.credentials,'same-origin');assert.equal(options.redirect,'error');assert.equal(options.cache,'no-store');
  assert.equal(options.headers.Authorization,undefined);
  if(url.endsWith('/auth/session'))return session();
  if(options.method==='POST')assert.equal(options.headers['X-CSRF-Token'],csrf);
  return json({name:'workspace'},options.method==='POST'?201:200);
 });
 assert.equal((await api.request('POST','/api/v1/workspaces',{name:'workspace'})).status,201);
 assert.equal(calls[1].options.body,'{"name":"workspace"}');
 assert.equal((await api.request('GET','/api/v1/workspaces?search=two%20words')).body.name,'workspace');
 assert.equal(calls.length,3);
 assert.equal((await api.session()).csrf_token,undefined);
});
test('invalid paths, methods, bodies, and caller headers fail before network access',async()=>{
 let calls=0;const api=client(async()=>{calls++;return session();});
 for(const path of ['/auth/session','/api/v10/workspaces','//other.example/api/v1','https://console.example/api/v1','/api/v1/../private','/api/v1/%2e%2e/private','/api/v1/x%2Fy','/api/v1/%252e','/api/v1/x#fragment','/api/v1/%00','/api/v1/%zz'])await assert.rejects(api.request('POST',path,{}),error('invalid_input'));
 await assert.rejects(api.request('OPTIONS','/api/v1/workspaces'),error('invalid_input'));
 await assert.rejects(api.request('GET','/api/v1/workspaces',{}),error('invalid_input'));
 await assert.rejects(api.request('POST','/api/v1/workspaces',{}, {headers:{Authorization:'private'}}),error('invalid_input'));
 await assert.rejects(api.request('POST','/api/v1/workspaces',{data:'x'.repeat((1<<20)+1)}),error('invalid_input'));
 assert.equal(calls,0);
});
test('expired sessions prevent writes and errors do not expose private data',async()=>{
 let calls=0;let api=client(async()=>{calls++;return json({authenticated:false,roles:[]});});
 await assert.rejects(api.request('DELETE','/api/v1/workspaces/one'),error('reauth_required'));assert.equal(calls,1);
 api=client(async()=>json({message:'private-response'},403));
 await assert.rejects(api.request('GET','/api/v1/workspaces'),e=>error('http_error')(e)&&e.status===403);
 api=client(async()=>new Response('{"name":"one","name":"two"}',{headers:{'Content-Type':'application/json'}}));
 await assert.rejects(api.request('GET','/api/v1/workspaces'),error('invalid_response'));
});
test('logout opens confirmation without a request and cancellation prevents work',async()=>{
 let calls=0;const api=client(async()=>{calls++;return session();});
 let address;globalThis.location.assign=value=>{address=value;};
 api.logout();assert.equal(address,'https://console.example/auth/logout');
 const controller=new AbortController();controller.abort();
 await assert.rejects(api.request('POST','/api/v1/workspaces',{}, {signal:controller.signal}),error('cancelled'));
 assert.equal(calls,0);
});
