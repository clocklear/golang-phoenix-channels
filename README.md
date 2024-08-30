# Phoenix Channels WebSocket Transport for Golang

This package provides a WebSocket transport with Phoenix Channels subprotocol support for the
[99designs/gqlgen](https://github.com/99designs/gqlgen) package.  This is very much a work in progress
and does not claim to be a complete implementation of the Phoenix Channels protocol, but merely enough
to support the needs of the project for which it was created.

## Why?

The project for which this was created has a frontend that was built using Phoenix LiveView,
which uses Phoenix Channels for its WebSocket transport.  The backend for the project is being reimplemented
in Go with the goal of being a drop-in replacement for the existing Elixir backend.  This package was
created to allow the Go backend to support the existing Phoenix Channels-based WebSocket transport
without requiring any changes to the frontend.

## You're Doing What Now?

The Phoenix Channels protocol is not a standard WebSocket subprotocol, so this package is not a
standard WebSocket transport for gqlgen.  It is a bespoke implementation of the Phoenix Channels
protocol that is intended to be used with the gqlgen package.  It should support the basic needs of
the project.

## You're crazy

I know.  But it's working so far, so I'm going to keep going with it.  Maybe one day we'll revamp the UI
to use a standard WebSocket subprotocol and I can throw this away.  But for now, it's what we've got.

## Ok, how do I use it?

If you're using gqlgen, you can add this package to your project and use it as a transport for your
subscriptions.  Here's an example of how you might do that:

```go
// Add the WSS transport to the server.
// In this excerpt, gqlSrv is an instance of a gqlgen server
// and opts is a struct that contains configuration options.
if opts.WSSUsePhoenix {
    // Use our custom Phoenix transport
    gqlSrv.AddTransport(&phoenix.Websocket{
        Upgrader: websocket.Upgrader{
            EnableCompression: opts.WSSEnableCompression,
        },
    })
} else {
    // Fallback to the default ws transport that has graphql-ws support
    gqlSrv.AddTransport(&transport.Websocket{
        KeepAlivePingInterval: opts.WSSKeepAlivePingInterval,
        Upgrader: websocket.Upgrader{
            EnableCompression: opts.WSSEnableCompression,
        },
    })
}
```
