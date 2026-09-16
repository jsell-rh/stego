import {createBrowserClient, SDKError} from './session/index.js';
const client=createBrowserClient();
const response=await client.request('POST','/api/v1/workspaces',{name:'one'},{signal:new AbortController().signal});
const body:unknown=response.body;
// @ts-expect-error Domain response fields require application validation.
const name:string=response.body.name;
// @ts-expect-error Callers cannot supply credentials or arbitrary headers.
await client.request('GET','/api/v1/workspaces',undefined,{headers:{Authorization:'secret'}});
// @ts-expect-error Unsupported methods must not compile.
await client.request('CONNECT','/api/v1');
client.logout();
void [body, name, SDKError];
