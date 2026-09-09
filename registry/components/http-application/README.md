This component generates an HTTP client for one HTTPS origin. It verifies TLS,
sets request and connection limits, and rejects redirects. The application
supplies its CA file and endpoint. Each request has a five-second deadline.

`Stream` reads newline-delimited frames through a GET request. It uses the same
origin, TLS, header, and connection controls as `Do`. A stream has a five-minute
deadline, a 4 MiB frame limit, and a 64 MiB total limit. Requests and streams share
16 request slots. The callback receives frames in order. Frame data is valid
only until the callback returns. The callback must honor its context.

A stream can fail after it delivered frames. The application protocol must
retain a cursor if it needs to resume. The client does not retry requests.
`Close` cancels active requests and streams and prevents new requests.
