import {createBrowserTelemetry, RootTraceIdGenerator} from './telemetry/index.js';
const telemetry = createBrowserTelemetry({sampleRatio:0.5,reportDeliveryFailure: failure => { const signal: 'logs'|'metrics'|'traces' = failure.signal; void signal; }});
telemetry.beginTrace(new RootTraceIdGenerator().generateTraceId());
telemetry.tracer.startSpan('workflow.started').end();
telemetry.logger.emit({body:'workflow.started',severityNumber:9});
telemetry.meter.createCounter('workflow.started').add(1);
await telemetry.forceFlush();
await telemetry.shutdown();
