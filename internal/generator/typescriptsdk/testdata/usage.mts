import {createBrowserClient, type RequestSchemas, type Client} from './sdk/index.js';
const client: Client = createBrowserClient();
const body: RequestSchemas['Record'] = {id: 'r1', name: 'One', count: 0, enabled: false, description: null};
const response = await client.createRecord({body});
const name: string = response.body.name;
await client.getRecord({id: 'r1', search: 'One'}, {signal: new AbortController().signal, traceparent: '00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01'});
await client.deleteRecord({id: 'r1'});
// @ts-expect-error A required path argument is absent.
client.getRecord();
// @ts-expect-error The body has an incorrect field type.
client.createRecord({body: {...body, count: 'zero'}});
// @ts-expect-error OAuth credentials cannot be supplied by browser code.
client.getRecord({id: 'r1'}, {token: 'private'});
void name;

// @ts-expect-error Response-only fields cannot be sent.
client.createRecord({body: {id: "r", name: "x", count: 0, enabled: false, server_id: "server"}});
