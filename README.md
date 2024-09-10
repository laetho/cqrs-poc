# cqrs-poc

This is a little experiment on building a streaming frontend with HTMX on the client. Leveraging SSE (Server Sent Events) from the backend. 

We do CQRS. The SSE events for command and query results are backed by NATS. All commands and queries gets published on diffrent work queues for processing. Result of commands and queries are streamed back to the client on /stream/commands and /stream/queries respectively.

## Status

Currently a very naive implementation, no actual commands or queries are implemented, but they are "processed" and streamed to the client. Will be interesting to see how flexible htmx + some js will be targeting different parts of the app based on command and query types.
